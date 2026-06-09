// Package pty wraps a process in a pseudo-terminal and fans its output out to
// any number of attached subscribers.
//
// The same machinery backs two callers:
//
//   - An interactive `/v1/exec-stream` request with `tty: true`: a fresh
//     process is started in a pty, one subscriber (the SSE handler) attaches,
//     and the session ends when the process exits.
//   - The sandbox entrypoint, when the config sets `tty: true`: sandboxd wraps
//     the long-lived entrypoint in a pty Session whose output is published to
//     the event broker, and clients attach/detach over the lifetime of the
//     sandbox by calling `/v1/exec-stream` with an empty command.
//
// Keeping the pty wiring (Start) and the fan-out (Session) here lets both
// paths share one implementation rather than each re-deriving the runc/pty
// controlling-terminal dance.
package pty

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	cpty "github.com/creack/pty"
	"golang.org/x/term"
)

// stdinReleaseDelay caps how long client stdin is buffered before being
// flushed unconditionally, covering programs that read input before producing
// any output (so the gate would otherwise never open on first-output).
const stdinReleaseDelay = 2 * time.Second

// ringCap bounds the recent-output buffer replayed to a new subscriber. It
// gives a late attacher a little scrollback and, for the exec-stream case,
// guarantees output produced in the gap between NewSession and Attach is not
// lost. Sized for a screenful or two, not full session history.
const ringCap = 64 * 1024

// subBuffer is the per-subscriber channel depth: the slack a reader may fall
// behind before broadcast starts skipping frames for it. Sized generously so
// transient slowness is absorbed without dropping any output.
const subBuffer = 1024

// Start opens a pseudo-terminal, makes it cmd's controlling terminal, starts
// cmd, and returns the pty master. The caller owns the master: read its
// output, write stdin to it, resize it via [Session], and Close it when done.
//
// The pty slave is handed to cmd as stdin/stdout/stderr and put in raw mode so
// this outer pty is pure transport — the real line discipline is the one the
// program (or, for runc, the in-container pty) establishes. Setsid + Setctty
// make the slave the process's controlling terminal so resizing the master
// (TIOCSWINSZ) delivers SIGWINCH to the program.
func Start(cmd *exec.Cmd) (*os.File, error) {
	master, slave, err := cpty.Open()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		master.Close()
		slave.Close()
		return nil, fmt.Errorf("set raw: %w", err)
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	// The child dup'd the slave at exec; the parent no longer needs it.
	slave.Close()
	return master, nil
}

// Session is a running pty-backed process whose output is fanned out to any
// number of attached subscribers. Client stdin is gated until the program is
// ready (its first output, or stdinReleaseDelay) so input written before the
// program switches the terminal to raw mode isn't echoed back as type-ahead.
type Session struct {
	master *os.File
	stdin  *gatedWriter

	mu     sync.Mutex
	subs   map[int]*subscriber
	nextID int
	ring   [][]byte // recent output, replayed to new subscribers
	ringSz int
	closed bool
	done   chan struct{}
}

type subscriber struct {
	ch   chan []byte
	done chan struct{} // closed on detach (client-initiated or force-drop)
}

// NewSession starts reading master in the background. Each output chunk is
// handed to onOutput (may be nil) and broadcast to attached subscribers. The
// session closes — Done is signalled, no further output is delivered — when
// the master reaches EOF (the wrapped process exited or master was Closed).
func NewSession(master *os.File, onOutput func([]byte)) *Session {
	s := &Session{
		master: master,
		stdin:  &gatedWriter{w: master},
		subs:   map[int]*subscriber{},
		done:   make(chan struct{}),
	}
	relTimer := time.AfterFunc(stdinReleaseDelay, func() { _ = s.stdin.release() })
	go s.readLoop(onOutput, relTimer)
	return s
}

func (s *Session) readLoop(onOutput func([]byte), relTimer *time.Timer) {
	defer relTimer.Stop()
	buf := make([]byte, 4096)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			// Output means the program is up and has likely switched the
			// terminal to raw mode, so buffered client stdin is safe to deliver.
			_ = s.stdin.release()
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if onOutput != nil {
				onOutput(chunk)
			}
			s.broadcast(chunk)
		}
		if err != nil {
			break
		}
	}
	s.mu.Lock()
	s.closed = true
	s.subs = map[int]*subscriber{}
	s.mu.Unlock()
	close(s.done)
}

// broadcast appends chunk to the ring and delivers it to every current
// subscriber. Delivery is lossy and never blocks: if a subscriber is behind
// (its buffer is full) the frame is skipped for that subscriber only.
//
// This is the right trade-off for a live terminal shared by multiple viewers:
//
//   - Blocking on a slow subscriber would stall the read loop and pause the
//     pty for EVERY viewer (and a wedged viewer would hang the process).
//   - Dropping a slow subscriber would close its stream, which for the
//     entrypoint terminal is indistinguishable from the process exiting and
//     tears the viewer (and, through the shared upstream, every viewer) down.
//
// Skipping a frame just means a behind viewer misses some bytes; a terminal
// recovers visually on the next repaint. The read loop keeps draining the
// master so the process is never paused, and no stream is ever torn down.
func (s *Session) broadcast(chunk []byte) {
	s.mu.Lock()
	s.ring = append(s.ring, chunk)
	s.ringSz += len(chunk)
	for s.ringSz > ringCap && len(s.ring) > 1 {
		s.ringSz -= len(s.ring[0])
		s.ring = s.ring[1:]
	}
	subs := make([]*subscriber, 0, len(s.subs))
	for _, sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- chunk:
		case <-sub.done:
		default:
			// Subscriber is behind; skip this frame for it rather than block
			// the loop or tear its stream down.
		}
	}
}

// Attach registers a new subscriber. It returns the recent output to replay
// first (scrollback), a channel of subsequent live output, a channel closed if
// the subscriber is force-detached, and a detach func the caller must defer.
// ok is false if the session has already closed (process gone).
//
// The ring snapshot and registration happen under one lock with the read
// loop's append, so every chunk reaches the subscriber exactly once: either in
// the replayed ring or on the live channel, never both and never neither.
func (s *Session) Attach() (replay [][]byte, live <-chan []byte, detached <-chan struct{}, detach func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, nil, nil, false
	}
	id := s.nextID
	s.nextID++
	sub := &subscriber{ch: make(chan []byte, subBuffer), done: make(chan struct{})}
	s.subs[id] = sub

	replay = make([][]byte, len(s.ring))
	copy(replay, s.ring)

	var once sync.Once
	detach = func() {
		once.Do(func() {
			s.mu.Lock()
			if _, exists := s.subs[id]; exists {
				delete(s.subs, id)
				close(sub.done)
			}
			s.mu.Unlock()
		})
	}
	return replay, sub.ch, sub.done, detach, true
}

// Write sends p to the process's stdin (gated until the program is ready).
func (s *Session) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Resize sets the pty window size, which delivers SIGWINCH to the program.
//
// If the requested size equals the pty's current size, the kernel suppresses
// the SIGWINCH (TIOCSWINSZ only signals on an actual change), so the program
// won't repaint. That's the common case for a re-attaching or second client on
// the shared entrypoint tty: it connects at the same geometry the previous
// client left, gets only stale scrollback, and never sees a fresh screen. To
// fix that we wiggle the size (set it off by one column, then restore it) which
// forces a SIGWINCH and a full repaint at the correct final size.
func (s *Session) Resize(rows, cols uint16) error {
	if cur, err := cpty.GetsizeFull(s.master); err == nil &&
		cur.Rows == rows && cur.Cols == cols {
		nudge := cols + 1
		if cols > 1 {
			nudge = cols - 1
		}
		_ = cpty.Setsize(s.master, &cpty.Winsize{Rows: rows, Cols: nudge})
	}
	return cpty.Setsize(s.master, &cpty.Winsize{Rows: rows, Cols: cols})
}

// Done is closed when the session ends (the wrapped process exited).
func (s *Session) Done() <-chan struct{} { return s.done }

// Close tears down the pty master, which ends the read loop and the session.
func (s *Session) Close() error { return s.master.Close() }

// gatedWriter buffers writes until release, then flushes them in order and
// lets subsequent writes pass through to w. It gates client stdin against a
// pty whose program is still starting: input written before the program
// switches the terminal to raw mode would otherwise be echoed as type-ahead.
type gatedWriter struct {
	mu       sync.Mutex
	w        *os.File
	released bool
	buf      []byte
}

func (g *gatedWriter) Write(b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.released {
		g.buf = append(g.buf, b...)
		return len(b), nil
	}
	return g.w.Write(b)
}

// release flushes any buffered input and opens the gate. Idempotent.
func (g *gatedWriter) release() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return nil
	}
	g.released = true
	if len(g.buf) == 0 {
		return nil
	}
	_, err := g.w.Write(g.buf)
	g.buf = nil
	return err
}
