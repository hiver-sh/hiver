package guestsess

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hiver-sh/hiver/internal/vsockexec"
)

// pipeConn is an in-memory bidirectional frame connection: what the test writes
// as "host" is readable by the bridge, and vice versa.
type pipeConn struct {
	rd io.Reader // bridge reads host frames here
	wr io.Writer // bridge writes session output here
}

func (c pipeConn) Read(p []byte) (int, error)  { return c.rd.Read(p) }
func (c pipeConn) Write(p []byte) (int, error) { return c.wr.Write(p) }

// hostEnd is the test's side of a session attach: it can send stdin frames and
// collect stdout until the bridge returns.
type hostEnd struct {
	toBridge   *io.PipeWriter // host stdin → bridge
	fromBridge *io.PipeReader // bridge stdout → host
	mu         sync.Mutex
	out        []byte
	exit       *int
	done       chan struct{}
}

// attach spins up EnsureAttach in a goroutine and returns the host end. The
// bridge reads host frames from one pipe and writes session output to another.
func attach(t *testing.T, r *Registry, id string, spec Spec) *hostEnd {
	t.Helper()
	hostStdinR, hostStdinW := io.Pipe()  // host → bridge
	bridgeOutR, bridgeOutW := io.Pipe()  // bridge → host
	h := &hostEnd{toBridge: hostStdinW, fromBridge: bridgeOutR, done: make(chan struct{})}

	// Collect everything the bridge writes (stdout frames + a possible exit).
	go func() {
		for {
			tt, payload, err := vsockexec.ReadFrame(bridgeOutR)
			if err != nil {
				return
			}
			h.mu.Lock()
			switch tt {
			case vsockexec.FrameStdout:
				h.out = append(h.out, payload...)
			case vsockexec.FrameExit:
				code := 0
				h.exit = &code
			}
			h.mu.Unlock()
		}
	}()

	go func() {
		defer close(h.done)
		_ = r.EnsureAttach(id, spec, pipeConn{rd: hostStdinR, wr: bridgeOutW}, 40, 80)
		bridgeOutW.Close()
	}()
	return h
}

func (h *hostEnd) send(t *testing.T, b []byte) {
	t.Helper()
	if err := vsockexec.WriteFrame(h.toBridge, vsockexec.FrameStdin, b); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
}

// detach simulates the host connection dropping (resume): close the host's write
// end so the bridge's frame reader hits EOF.
func (h *hostEnd) detach() { h.toBridge.Close() }

// waitFor polls until the collected output contains want or the deadline passes.
func (h *hostEnd) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ok := containsStr(h.out, want)
		h.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	got := string(h.out)
	h.mu.Unlock()
	t.Fatalf("never saw %q in output; got %q", want, got)
}

func containsStr(b []byte, s string) bool {
	return len(s) == 0 || (len(b) >= len(s) && indexOf(string(b), s) >= 0)
}
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestWarmReattach is the core guarantee: a session survives a host detach and a
// re-attach reuses the SAME live process (proven by writing to its stdin across
// the gap and seeing the same `cat` echo it back).
func TestWarmReattach(t *testing.T) {
	r := New()
	r.environ = func() []string { return []string{"PATH=/usr/bin:/bin"} }
	// `cat` echoes its stdin back over the pty — a stand-in for a long-lived
	// interactive process.
	spec := Spec{Command: "cat", TTY: true}

	h1 := attach(t, r, "entrypoint", spec)
	h1.send(t, []byte("first\n"))
	h1.waitFor(t, "first")

	// Host drops (simulating a resume cutting the exec connection).
	h1.detach()
	select {
	case <-h1.done:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not return after detach")
	}

	// The process must still be alive: re-attach with the SAME spec and prove it
	// by echoing more input through the same `cat`.
	r.mu.Lock()
	e := r.sessions["entrypoint"]
	r.mu.Unlock()
	if e == nil {
		t.Fatal("session was reaped on detach — it must survive")
	}
	select {
	case <-e.exited:
		t.Fatal("process exited on detach — it must stay running")
	default:
	}

	h2 := attach(t, r, "entrypoint", spec)
	h2.send(t, []byte("second\n"))
	h2.waitFor(t, "second")
	r.mu.Lock()
	e2 := r.sessions["entrypoint"]
	r.mu.Unlock()
	if e2 != e {
		t.Fatal("re-attach relaunched the process despite an unchanged spec")
	}
	h2.detach()
}

// TestRelaunchOnSpecChange: a changed launch field (command) must kill the old
// process and start a fresh one.
func TestRelaunchOnSpecChange(t *testing.T) {
	r := New()
	r.environ = func() []string { return []string{"PATH=/usr/bin:/bin"} }

	h1 := attach(t, r, "entrypoint", Spec{Command: "cat", TTY: true})
	h1.send(t, []byte("ping\n"))
	h1.waitFor(t, "ping")
	r.mu.Lock()
	old := r.sessions["entrypoint"]
	r.mu.Unlock()
	h1.detach()
	<-h1.done

	// Different command → relaunch.
	h2 := attach(t, r, "entrypoint", Spec{Command: "echo CHANGED; cat", TTY: true})
	h2.waitFor(t, "CHANGED")
	r.mu.Lock()
	new := r.sessions["entrypoint"]
	r.mu.Unlock()
	if new == old {
		t.Fatal("spec change did not relaunch the session")
	}
	select {
	case <-old.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("old process was not killed on relaunch")
	}
	h2.detach()
}

// TestSpecEqual covers the reuse-vs-relaunch decision directly.
func TestSpecEqual(t *testing.T) {
	base := Spec{Command: "claude", Cwd: "/workspace", TTY: true, Env: map[string]string{"A": "1", "B": "2"}}
	same := Spec{Command: "claude", Cwd: "/workspace", TTY: true, Env: map[string]string{"B": "2", "A": "1"}}
	if !base.equal(same) {
		t.Error("identical specs (env order aside) should be equal")
	}
	for _, d := range []Spec{
		{Command: "claude2", Cwd: "/workspace", TTY: true, Env: base.Env},
		{Command: "claude", Cwd: "/other", TTY: true, Env: base.Env},
		{Command: "claude", Cwd: "/workspace", TTY: false, Env: base.Env},
		{Command: "claude", Cwd: "/workspace", TTY: true, Env: map[string]string{"A": "1"}},
		{Command: "claude", Cwd: "/workspace", TTY: true, Env: map[string]string{"A": "9", "B": "2"}},
	} {
		if base.equal(d) {
			t.Errorf("specs should differ: %+v", d)
		}
	}
}
