package proxy

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// pipeConn returns one end of a net.Pipe wrapped with a bufio.Reader, plus the
// peer end (kept alive/closeable by the caller). A live pipe with no pending
// write makes connAlive's probe time out → "alive", which is what we want for
// reuse tests.
func pipeConn(t *testing.T) (net.Conn, *bufio.Reader, net.Conn) {
	t.Helper()
	c, peer := net.Pipe()
	return c, bufio.NewReader(c), peer
}

// TestPoolIsolation is the security-critical test: a connection put by one source
// must NEVER be retrievable by another source (or another host/addr). This is the
// guarantee that co-tenant sandboxes on a shared proxy can't see each other.
func TestPoolIsolation(t *testing.T) {
	p := newUpstreamPool(false)
	cA, brA, peerA := pipeConn(t)
	defer peerA.Close()
	p.put("10.0.0.1", "example.com:443", "example.com", cA, brA)

	// Another sandbox (different srcIP) must not get A's connection.
	if c, _ := p.get("10.0.0.2", "example.com:443", "example.com"); c != nil {
		t.Fatal("ISOLATION BREACH: srcIP B retrieved a connection opened by srcIP A")
	}
	// Same source but different upstream identity must not cross either.
	if c, _ := p.get("10.0.0.1", "evil.com:443", "example.com"); c != nil {
		t.Fatal("connection reused for a different dial address")
	}
	if c, _ := p.get("10.0.0.1", "example.com:443", "evil.com"); c != nil {
		t.Fatal("connection reused for a different TLS host")
	}
	// The owning source, same identity, gets it back.
	if c, _ := p.get("10.0.0.1", "example.com:443", "example.com"); c != cA {
		t.Fatal("owning source did not get its own pooled connection back")
	}
}

// TestPoolSharedScope verifies pod scope: a connection put by one source IS
// reused by a different source (cross-sandbox reuse), and evictSource is a no-op
// (connections are pod-owned, not per-source). The upstream identity must still
// match. This is the deliberate isolation trade-off for the fast path.
func TestPoolSharedScope(t *testing.T) {
	p := newUpstreamPool(true)
	cA, brA, peerA := pipeConn(t)
	defer peerA.Close()
	p.put("10.0.0.1", "example.com:443", "example.com", cA, brA)

	// Different source reuses A's connection (pod scope).
	if c, _ := p.get("10.0.0.2", "example.com:443", "example.com"); c != cA {
		t.Fatal("pod scope: a different source did not reuse the pooled connection")
	}
	// Identity still partitions: a different host must not cross.
	p.put("10.0.0.1", "example.com:443", "example.com", cA, brA)
	if c, _ := p.get("10.0.0.9", "evil.com:443", "evil.com"); c != nil {
		t.Fatal("pod scope: connection reused for a different upstream identity")
	}
	// evictSource is a no-op in pod scope (siblings may still use the conn).
	p.evictSource("10.0.0.1")
	if c, _ := p.get("10.0.0.3", "example.com:443", "example.com"); c != cA {
		t.Fatal("pod scope: evictSource wrongly dropped a pod-shared connection")
	}
}

// TestPoolEvictSource ensures removing a sandbox drops only its connections.
func TestPoolEvictSource(t *testing.T) {
	p := newUpstreamPool(false)
	cA, brA, peerA := pipeConn(t)
	cB, brB, peerB := pipeConn(t)
	defer peerA.Close()
	defer peerB.Close()
	p.put("10.0.0.1", "h:443", "h", cA, brA)
	p.put("10.0.0.2", "h:443", "h", cB, brB)

	p.evictSource("10.0.0.1")

	if c, _ := p.get("10.0.0.1", "h:443", "h"); c != nil {
		t.Fatal("evicted source still had a pooled connection")
	}
	if c, _ := p.get("10.0.0.2", "h:443", "h"); c != cB {
		t.Fatal("evictSource wrongly dropped another source's connection")
	}
}

// TestPoolStale ensures connections past idleTTL are discarded (and closed), not
// handed out — protecting against reuse of conns the origin has since dropped.
func TestPoolStale(t *testing.T) {
	p := newUpstreamPool(false)
	p.idleTTL = time.Millisecond
	c, br, peer := pipeConn(t)
	defer peer.Close()
	p.put("s", "h:443", "h", c, br)
	time.Sleep(5 * time.Millisecond)

	if got, _ := p.get("s", "h:443", "h"); got != nil {
		t.Fatal("stale connection was returned for reuse")
	}
	// The stale conn should have been closed on discard.
	if err := c.SetReadDeadline(time.Now().Add(time.Millisecond)); err == nil {
		var b [1]byte
		if _, rerr := c.Read(b[:]); rerr == nil {
			t.Fatal("stale connection was not closed")
		}
	}
}

// TestPoolDeadConnNotReturned ensures a connection the peer has closed fails the
// liveness probe and is not handed out.
func TestPoolDeadConnNotReturned(t *testing.T) {
	p := newUpstreamPool(false)
	c, br, peer := pipeConn(t)
	p.put("s", "h:443", "h", c, br)
	peer.Close() // origin drops the connection

	if got, _ := p.get("s", "h:443", "h"); got != nil {
		t.Fatal("dead connection passed the liveness probe and was returned")
	}
}

// TestPoolCap ensures the per-key idle cap is enforced (bounds FDs/memory); the
// overflow connection is closed rather than retained.
func TestPoolCap(t *testing.T) {
	p := newUpstreamPool(false)
	p.maxIdlePer = 2
	var peers []net.Conn
	for i := 0; i < 3; i++ {
		c, br, peer := pipeConn(t)
		peers = append(peers, peer)
		p.put("s", "h:443", "h", c, br)
	}
	defer func() {
		for _, pr := range peers {
			pr.Close()
		}
	}()
	p.mu.Lock()
	n := len(p.idle[poolKey{srcIP: "s", dialAddr: "h:443", host: "h"}])
	p.mu.Unlock()
	if n != 2 {
		t.Fatalf("idle cap not enforced: got %d idle conns, want 2", n)
	}
}

// TestPoolBufferedNotPooled ensures a conn whose reader still holds buffered bytes
// (unclean framing) is closed rather than pooled — reuse would desync responses.
func TestPoolBufferedNotPooled(t *testing.T) {
	p := newUpstreamPool(false)
	c, br, peer := pipeConn(t)
	go peer.Write([]byte("leftover")) // unblock the synchronous pipe
	if _, err := br.Peek(1); err != nil {
		t.Fatalf("peek: %v", err)
	}
	if br.Buffered() == 0 {
		t.Fatal("setup: expected buffered bytes")
	}
	p.put("s", "h:443", "h", c, br)

	if got, _ := p.get("s", "h:443", "h"); got != nil {
		t.Fatal("connection with buffered (unclean) data was pooled")
	}
}

// TestPoolClosedAfterShutdown ensures puts after closeAll don't retain conns.
func TestPoolClosedAfterShutdown(t *testing.T) {
	p := newUpstreamPool(false)
	p.closeAll()
	c, br, peer := pipeConn(t)
	defer peer.Close()
	p.put("s", "h:443", "h", c, br)
	if got, _ := p.get("s", "h:443", "h"); got != nil {
		t.Fatal("closed pool retained a connection")
	}
}
