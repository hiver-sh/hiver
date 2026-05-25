//go:build linux

package proxy

import (
	"bufio"
	"log"
	"net"
	"net/http"
)

// handleTransparentHTTP serves one origin-form HTTP request. The Host header
// carries the agent's intended hostname (used for allowlist matching); the
// pre-redirect destination IP/port (recovered via SO_ORIGINAL_DST) is what
// we actually dial, so DNS happens once at the agent — we don't re-resolve.
func (p *Proxy) handleTransparentHTTP(c *net.TCPConn, br *bufio.Reader, origDst string) {
	req, err := http.ReadRequest(br)
	if err != nil {
		log.Printf("transparent http: read request error origDst=%s: %v", origDst, err)
		return
	}
	host := req.Host
	if host == "" {
		host = origDst
	}
	hostOnly, _ := splitHostPort("", host, 0)
	// The destination port comes from SO_ORIGINAL_DST, not the Host header —
	// that's what the kernel will actually dial, and what we hold the agent to.
	_, port := splitHostPort("", origDst, 0)

	log.Printf("transparent http: host=%s port=%d method=%s path=%s ws=%v", hostOnly, port, req.Method, req.URL.Path, isWebSocketUpgrade(req))

	ac := p.beginAudit(req.Method, hostOnly, req.URL.Path, req.URL.RawQuery)
	if rule := p.applyRequestRule(req, hostOnly, port, ac, func() { writeDenyHTTP(c, hostOnly) }); rule == nil {
		return
	}

	upstream, err := p.dialer.DialContext(req.Context(), "tcp", origDst)
	if err != nil {
		log.Printf("transparent http: upstream dial error host=%s origDst=%s: %v", hostOnly, origDst, err)
		ac.responseError(err.Error(), http.StatusBadGateway)
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer upstream.Close()

	p.forwardHTTP(c, upstream, req, nil, ac)
}
