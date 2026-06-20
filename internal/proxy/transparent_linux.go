//go:build linux

// Transparent intercept entry point for the sandbox proxy. The flow is:
//
//	ServeTransparent → handleTransparent (per-conn dispatch)
//	                     ├── handleTransparentHTTP   (plain HTTP)
//	                     └── handleTransparentTLS    (peek SNI, route)
//	                            ├── rawForwardTLS    (no inspection — passthrough rules)
//	                            └── interceptTLS     (mint cert, inspect inner request)
//
// Both inspection paths converge on forwardHTTP, which handles the
// request → response → optional WebSocket upgrade lifecycle. Upstream TLS
// strategy lives in dialUpstreamTLS (Chrome JA3, with client-mirroring for
// WS so Cloudflare-protected hosts don't flag the proxy as a bot).
//
// Each piece of the flow lives in its own file in this package; see the
// sibling transparent_*_linux.go, forward_http_linux.go, upstream_tls_linux.go,
// and the small helper files (clienthello_linux.go, sniff_protocol_linux.go,
// peeked_conn_linux.go, ws_response_linux.go, original_dst_linux.go,
// tls_alert_linux.go).
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
)

// ServeTransparent runs a transparent intercept listener. iptables OUTPUT
// nat REDIRECTs all outbound TCP from agent processes to addr; the proxy
// recovers the pre-redirect destination via SO_ORIGINAL_DST and dispatches
// based on a peek of the first bytes:
//
//   - HTTP request line  → host-based allowlist + forward
//   - TLS ClientHello    → SNI-match; intercept (mint cert) or raw-forward
//   - Anything else      → denied
//
// Returns when ctx is cancelled.
func (p *Proxy) ServeTransparent(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("proxy: listen %s: %w", addr, err)
	}
	p.listener = l

	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go p.handleTransparent(c.(*net.TCPConn))
	}
}

func (p *Proxy) handleTransparent(c *net.TCPConn) {
	defer c.Close()

	origDst, err := getOriginalDst(c)
	if err != nil {
		// No SO_ORIGINAL_DST → connection wasn't redirected by iptables;
		// drop silently rather than guess at the intent.
		return
	}
	// If iptables didn't actually NAT this connection, SO_ORIGINAL_DST
	// returns the connection's actual destination (= our listening
	// address). That happens for sandboxd's own readiness probe and any
	// other direct dials to the proxy port. Don't audit those.
	if c.LocalAddr().String() == origDst {
		return
	}

	br := bufio.NewReaderSize(c, 4096)
	peek, _ := br.Peek(8)

	switch sniffProtocol(peek) {
	case protoHTTP:
		p.handleTransparentHTTP(c, br, origDst)
	case protoTLS:
		p.handleTransparentTLS(c, br, origDst)
	default:
		p.beginAudit(srcIPOf(c.RemoteAddr().String()), "?", origDst, "", "").deny("unknown protocol", 0)
	}
}
