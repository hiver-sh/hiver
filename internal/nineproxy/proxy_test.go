package nineproxy

import (
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

// kernelEnd drives the test's "kernel" side of the proxied transport: send a
// request frame and read the reply the proxy forwards back.
type kernelEnd struct{ c net.Conn }

func (k kernelEnd) req(t *testing.T, msg []byte) []byte {
	t.Helper()
	done := make(chan []byte, 1)
	go func() {
		if _, err := k.c.Write(msg); err != nil {
			return
		}
		resp, err := ReadMsg(k.c)
		if err != nil {
			return
		}
		done <- resp
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for proxied reply")
		return nil
	}
}

// TestProxyReconnect is the end-to-end guarantee: a workspace 9p session is
// proxied, the host connection dies (a resume), Reconnect replays the session
// onto a fresh host, and subsequent kernel traffic flows over it — all without
// the kernel transport ever closing.
func TestProxyReconnect(t *testing.T) {
	kKernel, kProxy := net.Pipe() // test kernel end / proxy kernel end
	h1Proxy, h1Srv := net.Pipe()

	p := NewProxy(kProxy, h1Proxy)
	go p.Run()
	defer p.Close()
	serveFake(h1Srv)
	k := kernelEnd{c: kKernel}

	// Establish a session through the live proxy.
	if got := msgType(k.req(t, frame(tTversion, notag, func(w *builder) { w.u32(262144); w.str("9p2000.L") }))); got != tRversion {
		t.Fatalf("version reply type %d", got)
	}
	k.req(t, frame(tTattach, 1, func(w *builder) {
		w.u32(1)
		w.u32(nofidVal)
		w.str("agent")
		w.str("workspace")
		w.u32(0)
	}))
	k.req(t, frame(tTwalk, 2, func(w *builder) {
		w.u32(1)
		w.u32(2)
		w.u16(2)
		w.str("home")
		w.str("agent")
	}))

	// The host connection dies (resume cuts it).
	h1Srv.Close()

	// Re-bind a fresh host and reconnect; replay must re-establish the session.
	h2Proxy, h2Srv := net.Pipe()
	fs2 := serveFake(h2Srv)
	if err := p.Reconnect(h2Proxy); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if fs2.attachFid != 1 {
		t.Errorf("reconnect did not re-attach root fid: %d", fs2.attachFid)
	}
	if got := fs2.walks[2]; !reflect.DeepEqual(got, []string{"home", "agent"}) {
		t.Errorf("reconnect did not re-walk fid2: %v", got)
	}

	// New kernel traffic must now flow over the fresh host: clunk fid 2.
	if got := msgType(k.req(t, frame(tTclunk, 9, func(w *builder) { w.u32(2) }))); got != tRclunk {
		t.Errorf("post-reconnect clunk reply type %d, want %d", got, tRclunk)
	}
}

// swallowConn wraps the proxy's host connection to reproduce how a resume
// actually loses a request: after Swallow(), writes SUCCEED and the bytes
// evaporate — exactly what a TCP send into a dead-but-not-yet-RST'd connection
// does. Reads keep flowing to the underlying conn (they error once the peer
// closes, which is what tells the proxy the host died).
type swallowConn struct {
	net.Conn
	mu        sync.Mutex
	swallow   bool
	swallowed chan struct{} // signalled once per swallowed write
}

func (s *swallowConn) Write(p []byte) (int, error) {
	s.mu.Lock()
	sw := s.swallow
	s.mu.Unlock()
	if sw {
		select {
		case s.swallowed <- struct{}{}:
		default:
		}
		return len(p), nil
	}
	return s.Conn.Write(p)
}

func (s *swallowConn) Swallow() {
	s.mu.Lock()
	s.swallow = true
	s.mu.Unlock()
}

// tTgetattr is a type the session parser passes through opaquely — the proxy
// must survive losing those too, not just the fid-mutating types it parses.
const tTgetattr = 24

// TestProxyReconnectResendsSwallowedRequest is the huge-pages turn wedge as a
// test: the guest issues a request in the window between the VM resuming and
// the dead host connection's RST arriving. The write "succeeds" into the doomed
// socket and the bytes evaporate; kernel v9fs then waits forever on the tag
// (p9_client_rpc, D state — the wedged `claude` child). Reconnect must
// re-deliver the swallowed request so the waiting process is released.
func TestProxyReconnectResendsSwallowedRequest(t *testing.T) {
	kKernel, kProxy := net.Pipe()
	h1Proxy, h1Srv := net.Pipe()
	h1 := &swallowConn{Conn: h1Proxy, swallowed: make(chan struct{}, 1)}

	p := NewProxy(kProxy, h1)
	go p.Run()
	defer p.Close()
	serveFake(h1Srv)
	k := kernelEnd{c: kKernel}

	// Live session: version + attach.
	if got := msgType(k.req(t, frame(tTversion, notag, func(w *builder) { w.u32(262144); w.str("9p2000.L") }))); got != tRversion {
		t.Fatalf("version reply type %d", got)
	}
	k.req(t, frame(tTattach, 1, func(w *builder) {
		w.u32(1)
		w.u32(nofidVal)
		w.str("agent")
		w.str("workspace")
		w.u32(0)
	}))

	// Resume: the host conn is dead but the guest doesn't know yet. The next
	// request is swallowed — written "successfully" into the void.
	h1.Swallow()
	reply := make(chan []byte, 1)
	go func() {
		if _, err := kKernel.Write(frame(tTgetattr, 7, func(w *builder) { w.u32(1); w.u32(0x7ff); w.u32(0) })); err != nil {
			return
		}
		if r, err := ReadMsg(kKernel); err == nil {
			reply <- r
		}
	}()
	select {
	case <-h1.swallowed:
	case <-time.After(3 * time.Second):
		t.Fatal("request never reached the (swallowing) host conn")
	}
	// Now the RST equivalent lands: the old host read side dies, pumps park.
	h1Srv.Close()

	// Reconnect onto a fresh host; replay must re-establish the session AND
	// re-deliver the swallowed request.
	h2Proxy, h2Srv := net.Pipe()
	fs2 := serveFake(h2Srv)
	if err := p.Reconnect(h2Proxy); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	// The kernel's wait on tag 7 must complete — this is the wedge assertion.
	select {
	case r := <-reply:
		if msgType(r) != tTgetattr+1 || msgTag(r) != 7 {
			t.Fatalf("reply type %d tag %d, want %d/7", msgType(r), msgTag(r), tTgetattr+1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kernel never got a reply for the swallowed request — guest process would be parked in p9_client_rpc forever")
	}
	fs2.mu.Lock()
	sawGetattr := len(fs2.opaque) > 0 && fs2.opaque[0] == tTgetattr
	fs2.mu.Unlock()
	if !sawGetattr {
		t.Errorf("new host never received the re-delivered request (opaque=%v)", fs2.opaque)
	}
}
