//go:build linux

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
)

// handleTransparentTLS reads enough of the TLS ClientHello to extract the
// SNI hostname and matches it host-only against the allowlist. What happens
// next depends on whether the proxy was given a CA:
//
//   - With a CA (and rule isn't Passthrough): interceptTLS mints a leaf cert,
//     terminates the agent's TLS, reads the inner HTTP request, applies
//     method/path/header rules, and proxies a separate TLS connection
//     upstream. Audit log gets full request/response visibility.
//
//   - Without a CA, or for Passthrough rules: rawForwardTLS pipes the byte
//     stream end-to-end after the host-only allow decision. Audit visibility
//     is reduced to host + port; no inner method/path/status. Pinning hosts
//     that can't tolerate MITM land here.
//
// The agent must trust the sandbox CA — sandboxd installs it into the
// agent rootfs at bundle prep time.
func (p *Proxy) handleTransparentTLS(c *net.TCPConn, br *bufio.Reader, origDst string) {
	peeked, _ := peekTLSRecord(br)
	// peekTLSRecord returns a view into the bufio buffer; copy before any
	// subsequent read invalidates it. The bytes are reused later to mirror
	// the client's TLS fingerprint upstream.
	hello := append([]byte(nil), peeked...)
	host := parseSNI(hello)
	log.Printf("transparent tls: origDst=%s sni=%q", origDst, host)
	if host == "" {
		host = origDst
	}
	_, port := splitHostPort("", origDst, 0)
	rule := MatchEgress(p.currentRules(), "TLS", host, port, "")
	if rule == nil || rule.Access == "deny" {
		log.Printf("transparent tls: host=%s port=%d denied (no matching rule)", host, port)
		p.beginAudit("TLS", host, "", "").deny("no matching rule", 0)
		// Send a fatal TLS Alert so the peer surfaces a concrete error
		// ("tlsv1 alert access denied") instead of the bare connection close
		// it would otherwise see as `SSL_ERROR_SYSCALL`.
		writeTLSAlert(c, tlsAlertFatal, tlsAlertAccessDenied)
		return
	}
	log.Printf("transparent tls: host=%s port=%d allowed minter=%v passthrough=%v", host, port, p.minter != nil, rule.Passthrough)
	if p.minter != nil && !rule.Passthrough {
		p.interceptTLS(c, br, host, origDst, hello)
		return
	}
	p.rawForwardTLS(c, br, host, origDst, rule)
}

// rawForwardTLS pipes the agent's TLS stream straight to the upstream without
// touching the cipher. Used when the rule sets Passthrough, or when no CA is
// configured. The dial target is the SNI host (origDst is a sinkhole
// placeholder); see upstreamAddr. An override.host on the rule redirects the
// TCP target only — the TLS inside is end-to-end, so the override target must
// present a cert the agent trusts for the original hostname. No audit
// visibility beyond host + port.
func (p *Proxy) rawForwardTLS(c *net.TCPConn, br *bufio.Reader, host, origDst string, rule *EgressRule) {
	dialAddr := dialTarget(rule, upstreamAddr(host, origDst))
	log.Printf("transparent tls: raw forward host=%s dialAddr=%s origDst=%s", host, dialAddr, origDst)
	upstream, err := p.dialer.DialContext(context.Background(), "tcp", dialAddr)
	if err != nil {
		p.beginAudit("TLS", host, "", "").deny("upstream dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, upstream); done <- struct{}{} }()
	<-done
}

// interceptTLS terminates the agent's TLS with a minted leaf cert, reads the
// inner HTTP request, applies HTTP-level rules, and forwards over a fresh
// upstream TLS connection. WebSocket-aware: upstream TLS for WS upgrades
// mirrors the agent's TLS fingerprint (so CF doesn't see Go's JA3 paired
// with non-browser HTTP headers); regular HTTPS uses Chrome's JA3.
func (p *Proxy) interceptTLS(c *net.TCPConn, br *bufio.Reader, host, origDst string, clientHello []byte) {
	clientTLS, err := p.acceptInnerTLS(c, br, host)
	if err != nil {
		log.Printf("intercept tls: inner handshake error host=%s: %v", host, err)
		p.beginAudit("TLS", host, "", "").deny("inner handshake: "+err.Error(), 0)
		return
	}
	defer clientTLS.Close()
	cs := clientTLS.ConnectionState()
	log.Printf("intercept tls: inner handshake ok host=%s version=0x%04x cipher=0x%04x", host, cs.Version, cs.CipherSuite)

	// Tee the request bytes off the wire so we can replay them verbatim
	// upstream on a WS upgrade. Preserves the agent's original header casing
	// and ordering, which some upstreams (e.g. Cloudflare) treat as a
	// fingerprint signal.
	var rawReqBuf bytes.Buffer
	req, err := http.ReadRequest(bufio.NewReader(io.TeeReader(clientTLS, &rawReqBuf)))
	if err != nil {
		log.Printf("intercept tls: read request error host=%s: %v", host, err)
		p.beginAudit("TLS", host, "", "").deny("read request: "+err.Error(), 0)
		return
	}
	log.Printf("intercept tls: request host=%s method=%s path=%s ws=%v", host, req.Method, req.URL.Path, isWebSocketUpgrade(req))

	_, port := splitHostPort("", origDst, 0)
	ac := p.beginAudit(req.Method, host, req.URL.Path, req.URL.RawQuery)
	rule := p.applyRequestRule(req, host, port, ac, func() {
		_, _ = clientTLS.Write([]byte("HTTP/1.1 403 Forbidden\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
	})
	if rule == nil {
		return
	}

	upstreamConn, err := p.dialUpstreamTLS(dialTarget(rule, upstreamAddr(host, origDst)), host, clientHello, isWebSocketUpgrade(req))
	if err != nil {
		log.Printf("intercept tls: upstream tls error host=%s origDst=%s: %v", host, origDst, err)
		ac.responseError("upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstreamConn.Close()

	var rawReqBytes []byte
	if isWebSocketUpgrade(req) && (rule.Override == nil || rule.Override.PrefixPath == "") {
		// The verbatim capture still carries the agent's original path, so
		// a prefix_path rewrite can't use it — fall back to req.Write at
		// the cost of the agent's exact header casing/ordering.
		rawReqBytes = rawReqBuf.Bytes()
	}
	p.forwardHTTP(clientTLS, upstreamConn, req, rawReqBytes, ac)
}

// acceptInnerTLS completes the agent-facing TLS handshake using a per-host
// leaf cert minted by the sandbox CA. ALPN is pinned to http/1.1 — the
// inspection loop reads request lines, not HPACK frames, so negotiating h2
// would silently break inspection.
func (p *Proxy) acceptInnerTLS(c *net.TCPConn, br *bufio.Reader, host string) (*tls.Conn, error) {
	cfg := &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.minter.Mint(host)
		},
		NextProtos: []string{"http/1.1"},
	}
	clientTLS := tls.Server(&peekedConn{Conn: c, r: br}, cfg)
	if err := clientTLS.HandshakeContext(context.Background()); err != nil && err != io.EOF {
		return nil, err
	}
	return clientTLS, nil
}
