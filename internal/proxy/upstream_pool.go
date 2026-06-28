package proxy

import (
	"bufio"
	"context"
	"net"
	"sync"
	"time"
)

// upstreamPool reuses keep-alive upstream connections across requests so the
// proxy doesn't pay a fresh TCP + TLS handshake (the dominant per-request cost)
// every time. It is the warm-connection half of the MITM fast path.
//
// SECURITY — per-source isolation is the whole point of the key design.
// One sbxproxy is shared by every sandbox in a pod, but each sandbox egresses
// from its own source IP (the netns SNAT). The pool key LEADS WITH srcIP, so a
// connection opened on behalf of sandbox A (srcIP_A) lives in a different bucket
// than anything sandbox B (srcIP_B) can ever look up — B can neither reuse A's
// socket nor observe it. This also preserves egress-policy integrity: a pooled
// connection is only ever handed back to the same source whose rules authorised
// it, so B can't ride a connection A opened to a host B isn't allowed to reach.
// host and dialAddr are in the key too, so a connection is only reused for the
// exact same upstream and TLS identity it was established for.
type upstreamPool struct {
	idleTTL    time.Duration // an idle conn older than this is discarded, not reused
	maxIdlePer int           // cap on idle conns retained per key (bounds FDs/memory)

	mu     sync.Mutex
	idle   map[poolKey][]*pooledConn // LIFO stacks (hottest conn reused first)
	closed bool
}

// poolKey partitions the pool. srcIP FIRST and ALWAYS: it is the isolation
// boundary between co-tenant sandboxes (see type doc). dialAddr is the actual
// TCP target (override-aware); host is the TLS server name / minted-cert identity
// — both must match for a reuse to be correct.
type poolKey struct {
	srcIP    string
	dialAddr string
	host     string
}

// pooledConn is an idle upstream connection plus the buffered reader that was
// reading its responses. The reader MUST travel with the conn: it may hold bytes
// already pulled off the socket, so dropping it and re-wrapping the raw conn
// would lose framing. Only ever pooled with an empty buffer (see put).
type pooledConn struct {
	conn      net.Conn
	br        *bufio.Reader
	idleSince time.Time
}

func newUpstreamPool() *upstreamPool {
	return &upstreamPool{
		idleTTL:    10 * time.Second, // short: only reuse within a page's burst, before origins time out idle keep-alives
		maxIdlePer: 8,
		idle:       map[poolKey][]*pooledConn{},
	}
}

// get returns a live idle connection for (srcIP, dialAddr, host), or nil to tell
// the caller to dial fresh. It pops the most-recently-used conn, discarding any
// that are past idleTTL or that fail a liveness probe (origins silently close
// idle keep-alives). The srcIP in the key is what guarantees a caller only ever
// receives a connection opened for its own sandbox.
func (p *upstreamPool) get(srcIP, dialAddr, host string) (net.Conn, *bufio.Reader) {
	key := poolKey{srcIP: srcIP, dialAddr: dialAddr, host: host}
	for {
		p.mu.Lock()
		stack := p.idle[key]
		if p.closed || len(stack) == 0 {
			p.mu.Unlock()
			return nil, nil
		}
		pc := stack[len(stack)-1]
		p.idle[key] = stack[:len(stack)-1]
		p.mu.Unlock()

		if time.Since(pc.idleSince) > p.idleTTL || !connAlive(pc.conn) {
			_ = pc.conn.Close()
			continue // try the next-newest; older conns are even likelier dead
		}
		return pc.conn, pc.br
	}
}

// put returns a connection to the pool for reuse. It is dropped (closed) when the
// pool is closed, the per-key cap is reached, or the reader still holds buffered
// bytes (a non-empty buffer means the previous response wasn't cleanly framed, so
// reuse would desync the next response). Caller must only put a conn whose last
// response was fully drained and keep-alive-eligible.
func (p *upstreamPool) put(srcIP, dialAddr, host string, conn net.Conn, br *bufio.Reader) {
	if br.Buffered() != 0 {
		_ = conn.Close()
		return
	}
	key := poolKey{srcIP: srcIP, dialAddr: dialAddr, host: host}
	p.mu.Lock()
	if p.closed || len(p.idle[key]) >= p.maxIdlePer {
		p.mu.Unlock()
		_ = conn.Close()
		return
	}
	p.idle[key] = append(p.idle[key], &pooledConn{conn: conn, br: br, idleSince: time.Now()})
	p.mu.Unlock()
}

// evictSource closes every idle connection belonging to a source. Called when a
// sandbox goes away so its sockets don't linger in the shared proxy — defence in
// depth for the isolation boundary (and FD hygiene), on top of idleTTL reaping.
func (p *upstreamPool) evictSource(srcIP string) {
	p.mu.Lock()
	var drop []*pooledConn
	for key, stack := range p.idle {
		if key.srcIP == srcIP {
			drop = append(drop, stack...)
			delete(p.idle, key)
		}
	}
	p.mu.Unlock()
	for _, pc := range drop {
		_ = pc.conn.Close()
	}
}

// reap runs until ctx is done, periodically closing idle conns past idleTTL so a
// sandbox that goes quiet doesn't pin upstream sockets. On ctx done it closes
// everything and marks the pool closed (later puts close immediately).
func (p *upstreamPool) reap(ctx context.Context) {
	t := time.NewTicker(p.idleTTL)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.closeAll()
			return
		case <-t.C:
			p.sweep()
		}
	}
}

func (p *upstreamPool) sweep() {
	now := time.Now()
	var drop []*pooledConn
	p.mu.Lock()
	for key, stack := range p.idle {
		kept := stack[:0]
		for _, pc := range stack {
			if now.Sub(pc.idleSince) > p.idleTTL {
				drop = append(drop, pc)
			} else {
				kept = append(kept, pc)
			}
		}
		if len(kept) == 0 {
			delete(p.idle, key)
		} else {
			p.idle[key] = kept
		}
	}
	p.mu.Unlock()
	for _, pc := range drop {
		_ = pc.conn.Close()
	}
}

func (p *upstreamPool) closeAll() {
	p.mu.Lock()
	p.closed = true
	drop := p.idle
	p.idle = map[poolKey][]*pooledConn{}
	p.mu.Unlock()
	for _, stack := range drop {
		for _, pc := range stack {
			_ = pc.conn.Close()
		}
	}
}

// connAlive reports whether an idle keep-alive conn is still usable: a brief
// read with a deadline returns a timeout when the peer is alive and quiet, and
// EOF/error when it has closed the connection. Any unexpected pending byte on an
// idle keep-alive conn is treated as unusable (we can't push it back), so the
// conn is discarded rather than risk desyncing the next response.
func connAlive(c net.Conn) bool {
	if err := c.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
		return false
	}
	defer c.SetReadDeadline(time.Time{})
	var b [1]byte
	n, err := c.Read(b[:])
	if err != nil {
		ne, ok := err.(net.Error)
		return ok && ne.Timeout() // timeout => alive & quiet; anything else => dead
	}
	return n == 0
}
