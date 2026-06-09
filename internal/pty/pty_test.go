package pty

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// recvUntil accumulates chunks from ch until the accumulated output contains
// want, failing the test if that doesn't happen before timeout.
func recvUntil(t *testing.T, ch <-chan []byte, want string, timeout time.Duration) string {
	t.Helper()
	var acc []byte
	deadline := time.After(timeout)
	for {
		select {
		case b := <-ch:
			acc = append(acc, b...)
			if strings.Contains(string(acc), want) {
				return string(acc)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %q", want, acc)
		}
	}
}

// TestStartAllocatesControllingTTY verifies Start gives the process a real
// controlling terminal: `test -t 1` only succeeds when stdout is a tty.
func TestStartAllocatesControllingTTY(t *testing.T) {
	cmd := exec.Command("sh", "-c", "test -t 1 && printf ISATTY")
	master, err := Start(cmd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer master.Close()

	// Read until the process exits and the pty closes. The trailing read may
	// surface an EIO/EOF depending on platform; we only care about the bytes.
	out, _ := io.ReadAll(master)
	_ = cmd.Wait()
	if !strings.Contains(string(out), "ISATTY") {
		t.Fatalf("entrypoint did not see a tty on stdout; output = %q", out)
	}
}

// newTestSession wraps a bare pty pair (no process) in a Session. Writing to
// the returned slave simulates the wrapped process emitting output.
func newTestSession(t *testing.T, onOutput func([]byte)) (*Session, *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	sess := NewSession(master, onOutput)
	t.Cleanup(func() {
		sess.Close()
		slave.Close()
	})
	return sess, slave
}

// TestSessionLiveOutput: a subscriber attached before output sees it live.
func TestSessionLiveOutput(t *testing.T) {
	sess, slave := newTestSession(t, nil)

	_, live, _, detach, ok := sess.Attach()
	if !ok {
		t.Fatal("Attach returned ok=false on a fresh session")
	}
	defer detach()

	if _, err := slave.Write([]byte("LIVE")); err != nil {
		t.Fatalf("write: %v", err)
	}
	recvUntil(t, live, "LIVE", 2*time.Second)
}

// TestSessionReplay: output produced before a client attaches is replayed to
// it as scrollback. Using a first subscriber's live delivery as a barrier
// guarantees the chunk is in the ring before the second attach.
func TestSessionReplay(t *testing.T) {
	sess, slave := newTestSession(t, nil)

	_, live1, _, detach1, ok := sess.Attach()
	if !ok {
		t.Fatal("first Attach ok=false")
	}
	defer detach1()

	if _, err := slave.Write([]byte("SCROLLBACK")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Once sub1 has the chunk live, broadcast has appended it to the ring.
	recvUntil(t, live1, "SCROLLBACK", 2*time.Second)

	replay, _, _, detach2, ok := sess.Attach()
	if !ok {
		t.Fatal("second Attach ok=false")
	}
	defer detach2()

	var joined []byte
	for _, c := range replay {
		joined = append(joined, c...)
	}
	if !strings.Contains(string(joined), "SCROLLBACK") {
		t.Fatalf("replay missing prior output; replay = %q", joined)
	}
}

// TestSessionMultipleSubscribers: every attached subscriber receives output.
func TestSessionMultipleSubscribers(t *testing.T) {
	sess, slave := newTestSession(t, nil)

	_, live1, _, detach1, ok1 := sess.Attach()
	_, live2, _, detach2, ok2 := sess.Attach()
	if !ok1 || !ok2 {
		t.Fatal("Attach ok=false")
	}
	defer detach1()
	defer detach2()

	if _, err := slave.Write([]byte("FANOUT")); err != nil {
		t.Fatalf("write: %v", err)
	}
	recvUntil(t, live1, "FANOUT", 2*time.Second)
	recvUntil(t, live2, "FANOUT", 2*time.Second)
}

// TestSessionOnOutput: the onOutput callback observes the process's output
// (used by the entrypoint to publish stdio events and log).
func TestSessionOnOutput(t *testing.T) {
	got := make(chan []byte, 8)
	_, slave := newTestSession(t, func(b []byte) {
		got <- append([]byte(nil), b...)
	})

	if _, err := slave.Write([]byte("OBSERVED")); err != nil {
		t.Fatalf("write: %v", err)
	}
	recvUntil(t, got, "OBSERVED", 2*time.Second)
}

// TestSessionDetachStopsDelivery: a detached subscriber's done channel is
// closed and it stops receiving output.
func TestSessionDetachStopsDelivery(t *testing.T) {
	sess, slave := newTestSession(t, nil)

	_, _, detached, detach, ok := sess.Attach()
	if !ok {
		t.Fatal("Attach ok=false")
	}
	detach()

	select {
	case <-detached:
	case <-time.After(2 * time.Second):
		t.Fatal("detached channel not closed after detach()")
	}

	// Output after detach must not panic (no send on a removed subscriber).
	if _, err := slave.Write([]byte("AFTER")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
}

// TestSessionCloseSignalsDone: closing the session ends it (Done closes) and
// subsequent attaches fail.
func TestSessionCloseSignalsDone(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer slave.Close()
	sess := NewSession(master, nil)

	sess.Close()
	select {
	case <-sess.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after Close")
	}

	if _, _, _, _, ok := sess.Attach(); ok {
		t.Fatal("Attach returned ok=true after session close")
	}
}

// TestSessionResize: resizing an open session succeeds.
func TestSessionResize(t *testing.T) {
	sess, _ := newTestSession(t, nil)
	if err := sess.Resize(40, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
}

// TestSessionResizeSameSizeForcesRepaint: a resize to the size the pty is
// already at must still deliver a SIGWINCH so a re-attaching client gets a
// fresh repaint. The kernel suppresses SIGWINCH on an unchanged TIOCSWINSZ, so
// Resize wiggles the size to force one. The wrapped program traps SIGWINCH and
// emits a marker each time it fires.
func TestSessionResizeSameSizeForcesRepaint(t *testing.T) {
	// Install the trap, then signal readiness: SIGWINCH's default disposition is
	// "ignore", so a resize arriving before the trap is installed is silently
	// lost. Wait for READY before resizing to avoid that race.
	cmd := exec.Command("sh", "-c", `trap 'printf WINCH' WINCH; printf READY; while :; do sleep 0.05; done`)
	master, err := Start(cmd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess := NewSession(master, nil)
	t.Cleanup(func() { sess.Close(); _ = cmd.Process.Kill() })

	_, live, _, detach, ok := sess.Attach()
	if !ok {
		t.Fatal("Attach ok=false")
	}
	defer detach()
	recvUntil(t, live, "READY", 2*time.Second)

	// Establish a known size; this first resize is an actual change.
	if err := sess.Resize(40, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	recvUntil(t, live, "WINCH", 2*time.Second)

	// Resizing to the SAME size must still produce a SIGWINCH (the repaint
	// nudge). Without the wiggle the kernel would swallow it and this times out.
	if err := sess.Resize(40, 100); err != nil {
		t.Fatalf("Resize (same size): %v", err)
	}
	recvUntil(t, live, "WINCH", 2*time.Second)
}

// TestGatedWriterGates: writes before release are buffered (not visible to the
// underlying writer), then flushed in order on release, with later writes
// passing straight through.
func TestGatedWriterGates(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	g := &gatedWriter{w: w}

	if _, err := g.Write([]byte("ab")); err != nil {
		t.Fatalf("gated write: %v", err)
	}

	// Nothing should reach the pipe until release.
	read := make(chan []byte, 1)
	go func() {
		b := make([]byte, 2)
		if _, err := io.ReadFull(r, b); err == nil {
			read <- b
		}
	}()
	select {
	case b := <-read:
		t.Fatalf("read %q before release; expected gating", b)
	case <-time.After(150 * time.Millisecond):
	}

	if err := g.release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case b := <-read:
		if string(b) != "ab" {
			t.Fatalf("flushed %q, want %q", b, "ab")
		}
	case <-time.After(time.Second):
		t.Fatal("buffered input not flushed after release")
	}

	// After release, writes pass through immediately.
	if _, err := g.Write([]byte("cd")); err != nil {
		t.Fatalf("post-release write: %v", err)
	}
	b := make([]byte, 2)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatalf("read passthrough: %v", err)
	}
	if string(b) != "cd" {
		t.Fatalf("passthrough %q, want %q", b, "cd")
	}

	// release is idempotent.
	if err := g.release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}
