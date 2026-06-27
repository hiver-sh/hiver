package nineproxy

import (
	"net"
	"reflect"
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
