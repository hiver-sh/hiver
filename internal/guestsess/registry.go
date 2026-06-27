// Package guestsess is the in-guest registry of detachable terminal sessions.
//
// It is the piece that makes a `tty: true` entrypoint survive a snapshot resume
// warm. A tty entrypoint runs as a guest pty session bridged to the host over a
// TCP exec connection; that connection dies on a resume (new netns/tap/IP, old
// host listener gone). Without this registry the guest would reap the process
// when the connection drops and the host would relaunch it cold.
//
// Instead the registry — owned by the long-lived sbxguest agent, which itself
// survives in guest RAM across the snapshot — keeps each named session's process
// and pty alive across host disconnects, keyed by a stable id. The host
// re-attaches on resume (a fresh connection) and the live, warm process is
// re-bridged. The entrypoint is relaunched ONLY when the resume-time config
// changes a launch-determining field (command/cwd/env/tty); an unchanged spec
// reuses the running process.
//
// The fan-out (drain-on-detach, replay-on-attach, multi-subscriber) is reused
// from internal/pty.Session, the same machinery the host side uses.
package guestsess

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"

	"github.com/hiver-sh/hiver/internal/pty"
	"github.com/hiver-sh/hiver/internal/vsockexec"
)

// Spec is the launch-determining config for a named session. Two specs that are
// equal share one live process across re-attaches; a changed spec relaunches.
// It is exactly the set of fields the resume contract treats as immutable for a
// running workload — command (entrypoint+args), cwd, env, tty.
type Spec struct {
	Command string
	Cwd     string
	Env     map[string]string
	TTY     bool
}

func (a Spec) equal(b Spec) bool {
	if a.Command != b.Command || a.Cwd != b.Cwd || a.TTY != b.TTY || len(a.Env) != len(b.Env) {
		return false
	}
	for k, v := range a.Env {
		if b.Env[k] != v {
			return false
		}
	}
	return true
}

// envSlice renders the spec's env as a sorted KEY=VALUE slice merged onto base
// (base first, so the spec wins on conflicts). Sorted so the merge is
// deterministic.
func (a Spec) envSlice(base []string) []string {
	if len(a.Env) == 0 {
		return base
	}
	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := append([]string(nil), base...)
	for _, k := range keys {
		out = append(out, k+"="+a.Env[k])
	}
	return out
}

// entry is one live, detachable session: a pty-wrapped process kept alive across
// host disconnects. exited is closed once the process is reaped; code holds its
// exit status.
type entry struct {
	spec   Spec
	sess   *pty.Session
	cmd    *exec.Cmd
	exited chan struct{}
	code   int
}

// Registry holds the guest's named detachable sessions. The zero value is not
// usable; call New.
type Registry struct {
	mu       sync.Mutex
	sessions map[string]*entry

	// environ is the base environment a launched session inherits (the workload
	// env applied to the agent). Injectable for tests.
	environ func() []string
	// start opens a pty for cmd and starts it. Injectable for tests; defaults to
	// pty.Start.
	start func(*exec.Cmd) (*os.File, error)
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		sessions: map[string]*entry{},
		environ:  os.Environ,
		start:    pty.Start,
	}
}

// EnsureAttach ensures the session named id is running spec — launching it if
// absent, reusing the live process if the spec is unchanged, or relaunching if
// it changed — then bridges conn to it. The call returns when conn drops (the
// session is left running, detached) or the process exits (a terminal Exit frame
// is sent first). It never reaps a live process on a dropped connection: that is
// the whole point.
func (r *Registry) EnsureAttach(id string, spec Spec, conn io.ReadWriter, rows, cols uint16) error {
	e, err := r.ensure(id, spec)
	if err != nil {
		return err
	}
	if rows > 0 && cols > 0 {
		_ = e.sess.Resize(rows, cols)
	}
	return bridge(e.sess, e.exited, &e.code, conn)
}

// ensure returns the live entry for id, launching or relaunching as the spec
// dictates. Holds the registry lock only around the map mutation + launch.
func (r *Registry) ensure(id string, spec Spec) (*entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.sessions[id]; e != nil {
		select {
		case <-e.exited:
			// Process gone; fall through to a fresh launch.
		default:
			if e.spec.equal(spec) {
				return e, nil // warm reuse — the resume fast path
			}
			// Launch-determining field changed: kill the old process and relaunch.
			r.kill(e)
		}
	}
	e, err := r.launch(spec)
	if err != nil {
		return nil, err
	}
	r.sessions[id] = e
	return e, nil
}

// launch starts spec's command in a pty and wires a reaper that records the exit
// code and closes exited.
func (r *Registry) launch(spec Spec) (*entry, error) {
	cmd := exec.Command("sh", "-c", spec.Command)
	cmd.Dir = spec.Cwd
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	cmd.Env = spec.envSlice(r.environ())
	master, err := r.start(cmd)
	if err != nil {
		return nil, fmt.Errorf("guestsess: start: %w", err)
	}
	e := &entry{spec: spec, cmd: cmd, exited: make(chan struct{})}
	e.sess = pty.NewSession(master, nil)
	go func() {
		// Wait reaps the process; the pty Session ends when the master EOFs.
		e.code = waitCode(cmd)
		e.sess.Close()
		close(e.exited)
	}()
	return e, nil
}

// kill terminates a session's process group and closes its pty. Called when a
// changed spec forces a relaunch. Best-effort: the reaper goroutine still runs.
func (r *Registry) kill(e *entry) {
	if e.cmd.Process != nil {
		// Negative pid → the whole process group (Setsid made the child a group
		// leader), so a shell's children die with it.
		_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
	}
	e.sess.Close()
	<-e.exited
}

// waitCode waits for cmd and extracts a conventional exit code.
func waitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// decodeWinsize parses a FrameResize payload.
func decodeWinsize(payload []byte) (vsockexec.Winsize, error) {
	var ws vsockexec.Winsize
	err := json.Unmarshal(payload, &ws)
	return ws, err
}

// bridge pumps one host connection against a live session until the connection
// drops (returns nil — session left running) or the process exits (sends a final
// Exit frame). Output is replayed-then-live via Session.Attach; host frames are
// stdin/resize.
func bridge(sess *pty.Session, exited <-chan struct{}, code *int, conn io.ReadWriter) error {
	replay, live, detached, detach, ok := sess.Attach()
	if !ok {
		// Raced with exit; report the terminal status so the host doesn't hang.
		<-exited
		return vsockexec.WriteJSON(conn, vsockexec.FrameExit, vsockexec.Exit{Code: *code})
	}
	defer detach()

	// Host→session: stdin + resize, in a goroutine (it blocks on conn reads). A
	// read error means the host detached; closing connGone unblocks the writer.
	connGone := make(chan struct{})
	go func() {
		defer close(connGone)
		for {
			t, payload, err := vsockexec.ReadFrame(conn)
			if err != nil {
				return
			}
			switch t {
			case vsockexec.FrameStdin:
				_, _ = sess.Write(payload)
			case vsockexec.FrameResize:
				if ws, err := decodeWinsize(payload); err == nil {
					_ = sess.Resize(ws.Rows, ws.Cols)
				}
			}
		}
	}()

	for _, chunk := range replay {
		if err := vsockexec.WriteFrame(conn, vsockexec.FrameStdout, chunk); err != nil {
			return nil // host already gone; leave the session running
		}
	}
	for {
		select {
		case chunk, openCh := <-live:
			if !openCh {
				// Session output ended (process exiting). Drain to exit below.
				<-exited
				return vsockexec.WriteJSON(conn, vsockexec.FrameExit, vsockexec.Exit{Code: *code})
			}
			if err := vsockexec.WriteFrame(conn, vsockexec.FrameStdout, chunk); err != nil {
				return nil
			}
		case <-detached:
			return nil
		case <-connGone:
			return nil // host detached — leave the warm process running
		case <-exited:
			return vsockexec.WriteJSON(conn, vsockexec.FrameExit, vsockexec.Exit{Code: *code})
		}
	}
}
