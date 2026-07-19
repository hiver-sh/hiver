//go:build linux

package proxy

import (
	"bufio"
	"log"
	"net"
	"net/http"

	"github.com/hiver-sh/hiver/internal/wsaudit"
)

// handleTransparentHTTP serves one origin-form HTTP request. The Host header
// carries the agent's intended hostname, used both for allowlist matching and
// as the dial target: the agent's DNS is sinkholed, so origDst is a placeholder
// and the proxy re-resolves the name itself (see upstreamAddr). The destination
// port still comes from SO_ORIGINAL_DST.
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

	log.Printf("transparent http: host=%s port=%d method=%s path=%s ws=%v", hostOnly, port, req.Method, req.URL.Path, wsaudit.IsUpgrade(req))

	ac := p.beginAudit(srcIPOf(c.RemoteAddr().String()), req.Method, hostOnly, req.URL.Path, req.URL.RawQuery)
	rule := p.applyRequestRule(req, hostOnly, port, srcIPOf(c.RemoteAddr().String()), ac, func() { writeDenyHTTP(c, hostOnly) })
	if rule == nil {
		return
	}

	dialAddr := dialTarget(rule, upstreamAddr(host, origDst))
	upstream, err := p.dialer.DialContext(req.Context(), "tcp", dialAddr)
	if err != nil {
		log.Printf("transparent http: upstream dial error host=%s dialAddr=%s origDst=%s: %v", hostOnly, dialAddr, origDst, err)
		ac.responseError(err.Error(), http.StatusBadGateway)
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer upstream.Close()

	// Plain-HTTP transparent path: dial-per-request, no pooling (the dial is cheap
	// without a TLS handshake), so allowReuse is false and the conn is closed above.
	p.forwardHTTP(c, upstream, bufio.NewReader(upstream), req, nil, ac, false)
}
