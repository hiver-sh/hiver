//go:build linux

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"syscall"
	"unsafe"

	utls "github.com/refraction-networking/utls"
)

// soOriginalDst mirrors Linux's iptables nat-table flag. /usr/include/linux/netfilter_ipv4.h.
const soOriginalDst = 80

// ServeTransparent runs a transparent intercept listener. iptables OUTPUT
// nat REDIRECTs all outbound TCP from agent processes to addr; the proxy
// recovers the pre-redirect destination via SO_ORIGINAL_DST and dispatches
// based on a peek of the first bytes:
//
//   - HTTP request line  → existing host-based allowlist + forward
//   - TLS ClientHello    → denied (Phase 2 will SNI-match + raw-forward)
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
		p.beginAudit("?", origDst, "", "").deny("unknown protocol", 0)
	}
}

// peekTLSRecord returns the bytes of the next TLS record (header + body)
// without consuming them. We can't blindly Peek(1024) because a small
// ClientHello (e.g. curl 7.88 with a tight cipher list) may be only
// ~500 bytes, and bufio.Reader.Peek blocks waiting for the missing
// bytes that never arrive. Reading the 5-byte record header first
// gives us the exact length to ask for.
func peekTLSRecord(br *bufio.Reader) ([]byte, error) {
	hdr, err := br.Peek(5)
	if err != nil || len(hdr) < 5 {
		return nil, fmt.Errorf("short TLS record header: %w", err)
	}
	bodyLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	const maxRecord = 16384 + 5 // RFC 8446 §5.1 plaintext fragment cap + header
	total := 5 + bodyLen
	if total > maxRecord {
		return nil, fmt.Errorf("TLS record too large: %d", total)
	}
	full, err := br.Peek(total)
	if err != nil && len(full) < total {
		return nil, err
	}
	return full, nil
}

// handleTransparentTLS reads enough of the TLS ClientHello to extract
// the SNI hostname and matches it host-only against the allowlist.
// What happens next depends on whether the proxy was given a CA:
//
//   - With a CA, the proxy always terminates TLS: presents a leaf cert
//     minted by the sandbox CA, reads the inner HTTP request, applies
//     method/path/header rules from the matched rule, and proxies a
//     separate TLS connection upstream. This gives the audit log full
//     visibility (HTTP method, path, status, duration) for every
//     allowed flow, not just rules that carry inspection criteria.
//     The agent must trust the sandbox CA — sandboxd installs it into
//     the agent rootfs at bundle prep time.
//
//   - Without a CA, the proxy raw-forwards the byte stream end-to-end
//     after the host-only allow decision. Audit visibility is reduced
//     to host + port; no inner method/path/status. Pinning hosts that
//     can't tolerate MITM also land here when an operator deliberately
//     omits the CA.
func (p *Proxy) handleTransparentTLS(c *net.TCPConn, br *bufio.Reader, origDst string) {
	peeked, _ := peekTLSRecord(br)
	// peekTLSRecord returns a view into the bufio buffer; copy before any
	// subsequent read on br invalidates it. The bytes are reused later to
	// mirror the client's TLS fingerprint upstream.
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
		// ("tlsv1 alert access denied") instead of the bare connection
		// close it would otherwise see as `SSL_ERROR_SYSCALL`.
		writeTLSAlert(c, tlsAlertFatal, tlsAlertAccessDenied)
		return
	}
	log.Printf("transparent tls: host=%s port=%d allowed rule=%+v minter=%v passthrough=%v", host, port, rule.Access, p.minter != nil, rule.Passthrough)
	if p.minter != nil && !rule.Passthrough {
		p.interceptTLS(c, br, host, origDst, hello)
		return
	}
	p.rawForwardTLS(c, br, host, origDst)
}

func (p *Proxy) rawForwardTLS(c *net.TCPConn, br *bufio.Reader, host, origDst string) {
	log.Printf("raw tls forward: host=%s origDst=%s", host, origDst)
	upstream, err := p.dialer.DialContext(context.Background(), "tcp", origDst)
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

// interceptTLS terminates TLS, applies HTTP-level rules to the inner
// request, and proxies it to the upstream over a separate TLS
// connection. One request per connection (Connection: close) — the
// HTTP/1.1 keep-alive loop is a follow-up.
//
// The upstream TLS fingerprint is chosen after reading the inner HTTP
// request: WebSocket upgrades use standard TLS so that a browser-like
// JA3 hash is never paired with non-browser HTTP headers, which
// triggers Cloudflare Bot Management. Regular HTTPS uses the Chrome
// fingerprint as before to avoid JA3-based blocking.
func (p *Proxy) interceptTLS(c *net.TCPConn, br *bufio.Reader, host, origDst string, clientHello []byte) {
	log.Printf("intercept tls: host=%s origDst=%s", host, origDst)
	innerCfg := &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.minter.Mint(host)
		},
		// Force HTTP/1.1 — our inspection loop reads request lines,
		// not HPACK frames. Modern hosts negotiate h2 via ALPN; pinning
		// http/1.1 keeps the agent's stack on the version we can read.
		NextProtos: []string{"http/1.1"},
	}
	// Connection-level audit only emits on TLS failure / upstream-dial
	// failure (host-only, method "TLS"). On a clean handshake the HTTP-level
	// auditCtx below covers everything the consumer needs — emitting both
	// would double-log every successful request.
	clientTLS := tls.Server(&peekedConn{Conn: c, r: br}, innerCfg)
	if err := clientTLS.HandshakeContext(context.Background()); err != nil && err != io.EOF {
		log.Printf("intercept tls: client handshake error host=%s: %v", host, err)
		p.beginAudit("TLS", host, "", "").deny("inner handshake: "+err.Error(), 0)
		return
	}
	log.Printf("intercept tls: client handshake ok host=%s version=0x%04x cipher=0x%04x", host, clientTLS.ConnectionState().Version, clientTLS.ConnectionState().CipherSuite)
	defer clientTLS.Close()

	// Read the HTTP request before opening the upstream connection so we
	// can choose the upstream TLS fingerprint based on request type.
	var rawReqBuf bytes.Buffer
	req, err := http.ReadRequest(bufio.NewReader(io.TeeReader(clientTLS, &rawReqBuf)))
	if err != nil {
		log.Printf("intercept tls: read request error host=%s: %v", host, err)
		p.beginAudit("TLS", host, "", "").deny("read request: "+err.Error(), 0)
		return
	}
	log.Printf("intercept tls: request host=%s method=%s path=%s ws=%v headers=%v", host, req.Method, req.URL.Path, isWebSocketUpgrade(req), req.Header)
	ac := p.beginAudit(req.Method, host, req.URL.Path, req.URL.RawQuery)
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			log.Printf("interceptTLS: read body: %v", err)
		} else {
			req.Body = io.NopCloser(bytes.NewReader(b))
			ac.requestBody = decodeBytes(req.Header.Get("Content-Encoding"), b)
		}
	}
	ac.requestHeaders = headerMap(req.Header)
	_, port := splitHostPort("", origDst, 0)
	rule := MatchEgress(p.currentRules(), req.Method, host, port, req.URL.Path)
	if rule == nil || rule.Access == "deny" {
		log.Printf("intercept tls: request denied host=%s method=%s path=%s", host, req.Method, req.URL.Path)
		ac.deny("no matching rule", http.StatusForbidden)
		_, _ = clientTLS.Write([]byte("HTTP/1.1 403 Forbidden\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	if rule.Access == "allow" {
		applyOverride(req, rule.Override)
		ac.requestHeaders = headerMap(req.Header)
	}
	ac.allow()
	req.RequestURI = ""

	// WebSocket upgrades mirror the client's TLS fingerprint upstream so
	// Cloudflare Bot Management sees the same JA3/extension set as the
	// original client (e.g. native-tls/OpenSSL from a Rust client),
	// not Go's default TLS fingerprint. If mirroring fails (parse error
	// from FromRaw, or the upstream rejects the mirrored handshake), we
	// fall back to the Chrome fingerprint — the JA3 still looks
	// browser-like, just not byte-identical to the client.
	var upstreamConn net.Conn
	if isWebSocketUpgrade(req) {
		uc, err := dialMirroredWithChromeFallback(p.dialer, origDst, host, clientHello)
		if err != nil {
			log.Printf("intercept tls: upstream ws dial error host=%s origDst=%s: %v", host, origDst, err)
			ac.responseError("upstream: "+err.Error(), http.StatusBadGateway)
			return
		}
		upstreamConn = uc
	} else {
		rawUp, err := p.dialer.DialContext(context.Background(), "tcp", origDst)
		if err != nil {
			log.Printf("intercept tls: upstream dial error host=%s origDst=%s: %v", host, origDst, err)
			ac.responseError("upstream dial: "+err.Error(), http.StatusBadGateway)
			return
		}
		uc, err := chromeTLSHandshake(rawUp, host)
		if err != nil {
			log.Printf("intercept tls: upstream (chrome) handshake error host=%s: %v", host, err)
			ac.responseError("upstream handshake: "+err.Error(), http.StatusBadGateway)
			_ = rawUp.Close()
			return
		}
		cs := uc.ConnectionState()
		log.Printf("intercept tls: upstream (chrome) handshake ok host=%s version=0x%04x cipher=0x%04x", host, cs.Version, cs.CipherSuite)
		upstreamConn = uc
	}
	defer upstreamConn.Close()

	var writeErr error
	if isWebSocketUpgrade(req) {
		log.Printf("intercept tls: forwarding ws upgrade host=%s path=%s rawBytes=%d", host, req.URL.Path, rawReqBuf.Len())
		// Forward the raw captured bytes so header names keep their original
		// capitalization.
		_, writeErr = upstreamConn.Write(rawReqBuf.Bytes())
	} else {
		req.Header.Set("Connection", "close")
		// Prevent req.Write from injecting "User-Agent: Go-http-client/1.1"
		// when the agent didn't send one — that header fingerprints the proxy.
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header["User-Agent"] = []string{""}
		}
		writeErr = req.Write(upstreamConn)
	}
	if writeErr != nil {
		log.Printf("intercept tls: write request error host=%s: %v", host, writeErr)
		ac.responseError(writeErr.Error(), http.StatusBadGateway)
		return
	}
	upstreamBR := bufio.NewReader(upstreamConn)
	resp, err := http.ReadResponse(upstreamBR, req)
	if err != nil {
		log.Printf("intercept tls: read response error host=%s: %v", host, err)
		ac.responseError(err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("intercept tls: response host=%s status=%d", host, resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSwitchingProtocols {
		// Write 101 with RFC 6455 header casing. http.Header.Write
		// canonicalizes "Sec-WebSocket-Accept" → "Sec-Websocket-Accept",
		// which strict WS clients (notably tungstenite) reject with
		// "Key mismatch in 'Sec-WebSocket-Accept' header" or "Attack
		// attempt detected". Doing the response line + WS handshake
		// headers by hand sidesteps the canonicalizer.
		log.Printf("intercept tls: ws 101 switching protocols host=%s", host)
		if err := writeWebSocketResponse(clientTLS, resp); err != nil {
			ac.responseError("ws write response: "+err.Error(), http.StatusInternalServerError)
			return
		}
		done := make(chan struct{}, 2)
		go func() { p.wsForward(clientTLS, upstreamConn, ac); done <- struct{}{} }()
		go func() { p.wsForward(io.MultiReader(upstreamBR, upstreamConn), clientTLS, ac); done <- struct{}{} }()
		<-done
		ac.response(http.StatusSwitchingProtocols)
		return
	}

	src := unwrapBody(resp)
	ac.responseHeaders = headerMap(resp.Header)

	if err := writeResponseHeaders(clientTLS, resp); err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		return
	}
	streaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	p.chunkForward(src, clientTLS, nil, ac, streaming)
	ac.response(resp.StatusCode)
}

// writeWebSocketResponse writes a 101 Switching Protocols response to w
// using RFC 6455 header casing for the WebSocket handshake headers. Go's
// http.Header.Write canonicalizes "Sec-WebSocket-Accept" to
// "Sec-Websocket-Accept" (lower-case W/A) because CanonicalMIMEHeaderKey
// uppercases only the first letter of each '-' separated word. Per
// HTTP/1.1 header names are case-insensitive, but tungstenite (and other
// strict WS clients) match exact RFC casing on Sec-WebSocket-Accept and
// reject mismatches with "Key mismatch" / "Attack attempt detected".
// Writing the response by hand sidesteps the canonicalizer.
//
// Non-handshake headers (Date, Server, Cf-Ray, …) pass through with Go's
// canonical casing — clients only case-match the WS-spec headers, not the
// metadata.
func writeWebSocketResponse(w io.Writer, resp *http.Response) error {
	bw := bufio.NewWriter(w)
	statusText := http.StatusText(resp.StatusCode)
	if statusText == "" {
		statusText = "Switching Protocols"
	}
	if _, err := fmt.Fprintf(bw, "HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText); err != nil {
		return err
	}
	hdrs := resp.Header.Clone()
	hdrs.Del("Transfer-Encoding")
	rfcCasing := map[string]string{
		"sec-websocket-accept":     "Sec-WebSocket-Accept",
		"sec-websocket-protocol":   "Sec-WebSocket-Protocol",
		"sec-websocket-extensions": "Sec-WebSocket-Extensions",
		"sec-websocket-version":    "Sec-WebSocket-Version",
	}
	for canon, values := range hdrs {
		name := canon
		if rfc, ok := rfcCasing[strings.ToLower(canon)]; ok {
			name = rfc
		}
		for _, v := range values {
			if _, err := fmt.Fprintf(bw, "%s: %s\r\n", name, v); err != nil {
				return err
			}
		}
	}
	if _, err := io.WriteString(bw, "\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// peekedConn wraps a net.Conn so reads come from a bufio.Reader that
// still holds the peeked ClientHello bytes. tls.Server reads the
// handshake from this composite reader; writes go straight to the
// underlying conn.
type peekedConn struct {
	net.Conn
	r io.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// handleTransparentHTTP serves one origin-form HTTP request. The Host
// header carries the agent's intended hostname (used for allowlist
// matching); the original destination IP/port (recovered via
// SO_ORIGINAL_DST) is what we actually dial, so DNS happens once at the
// agent — we don't re-resolve.
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
	// The destination port comes from SO_ORIGINAL_DST, not the Host
	// header — that's what the kernel will actually dial, and what
	// the agent is being held to.
	_, port := splitHostPort("", origDst, 0)

	log.Printf("transparent http: host=%s port=%d method=%s path=%s ws=%v headers=%v", hostOnly, port, req.Method, req.URL.Path, isWebSocketUpgrade(req), req.Header)

	ac := p.beginAudit(req.Method, hostOnly, req.URL.Path, req.URL.RawQuery)
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			log.Printf("handleTransparentHTTP: read body: %v", err)
		} else {
			req.Body = io.NopCloser(bytes.NewReader(b))
			ac.requestBody = decodeBytes(req.Header.Get("Content-Encoding"), b)
		}
	}
	ac.requestHeaders = headerMap(req.Header)
	rule := MatchEgress(p.currentRules(), req.Method, hostOnly, port, req.URL.Path)
	if rule == nil || rule.Access == "deny" {
		log.Printf("transparent http: denied host=%s port=%d method=%s path=%s", hostOnly, port, req.Method, req.URL.Path)
		ac.deny("no matching rule", http.StatusForbidden)
		writeDenyHTTP(c, hostOnly)
		return
	}
	log.Printf("transparent http: allowed host=%s port=%d rule=%s", hostOnly, port, rule.Access)
	if rule.Access == "allow" {
		applyOverride(req, rule.Override)
		ac.requestHeaders = headerMap(req.Header)
	}
	ac.allow()

	// http.ReadRequest leaves req.RequestURI set; clear it so it doesn't
	// end up as proxy-form on the wire.
	req.RequestURI = ""

	upstream, err := p.dialer.DialContext(req.Context(), "tcp", origDst)
	if err != nil {
		log.Printf("transparent http: upstream dial error host=%s origDst=%s: %v", hostOnly, origDst, err)
		ac.responseError(err.Error(), http.StatusBadGateway)
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer upstream.Close()

	var writeErr error
	if isWebSocketUpgrade(req) {
		log.Printf("transparent http: forwarding ws upgrade host=%s path=%s", hostOnly, req.URL.Path)
		writeErr = writeWebSocketUpgrade(upstream, req)
	} else {
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header["User-Agent"] = []string{""}
		}
		writeErr = req.Write(upstream)
	}
	if writeErr != nil {
		log.Printf("transparent http: write request error host=%s: %v", hostOnly, writeErr)
		ac.responseError(writeErr.Error(), http.StatusBadGateway)
		return
	}
	upstreamBR := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamBR, req)
	if err != nil {
		log.Printf("transparent http: read response error host=%s: %v", hostOnly, err)
		ac.responseError(err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("transparent http: response host=%s status=%d", hostOnly, resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSwitchingProtocols {
		log.Printf("transparent http: ws 101 switching protocols host=%s", hostOnly)
		if err := resp.Write(c); err != nil {
			ac.responseError("ws write response: "+err.Error(), http.StatusInternalServerError)
			return
		}
		done := make(chan struct{}, 2)
		go func() { p.wsForward(c, upstream, ac); done <- struct{}{} }()
		go func() { p.wsForward(io.MultiReader(upstreamBR, upstream), c, ac); done <- struct{}{} }()
		<-done
		ac.response(http.StatusSwitchingProtocols)
		return
	}

	src := unwrapBody(resp)
	ac.responseHeaders = headerMap(resp.Header)

	if err := writeResponseHeaders(c, resp); err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		return
	}
	streaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	p.chunkForward(src, c, nil, ac, streaming)
	ac.response(resp.StatusCode)
}

type protocol int

const (
	protoUnknown protocol = iota
	protoHTTP
	protoTLS
)

// sniffProtocol detects HTTP request lines and TLS ClientHello records
// from a few peeked bytes.
func sniffProtocol(peek []byte) protocol {
	// TLS record: type=0x16 (handshake), version 0x03xx, ... handshake
	// type at offset 5 = 0x01 (ClientHello).
	if len(peek) >= 6 && peek[0] == 0x16 && peek[1] == 0x03 && peek[5] == 0x01 {
		return protoTLS
	}
	// HTTP request line begins with a known method followed by a space.
	for _, m := range [][]byte{
		[]byte("GET "), []byte("POST"), []byte("HEAD"),
		[]byte("PUT "), []byte("DELE"), []byte("OPTI"),
		[]byte("PATC"), []byte("TRAC"),
	} {
		if bytes.HasPrefix(peek, m) {
			return protoHTTP
		}
	}
	return protoUnknown
}

// getOriginalDst reads SO_ORIGINAL_DST off a redirected TCP connection.
// Returns "ip:port" suitable for net.Dial.
func getOriginalDst(c *net.TCPConn) (string, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return "", err
	}
	var addr string
	var sockErr error
	ctlErr := rc.Control(func(fd uintptr) {
		var raw [16]byte // sizeof(sockaddr_in)
		size := uint32(len(raw))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(syscall.SOL_IP),
			uintptr(soOriginalDst),
			uintptr(unsafe.Pointer(&raw[0])),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			sockErr = errno
			return
		}
		// struct sockaddr_in: family(2) | port(2 BE) | addr(4) | pad(8)
		port := binary.BigEndian.Uint16(raw[2:4])
		ip := net.IPv4(raw[4], raw[5], raw[6], raw[7])
		addr = fmt.Sprintf("%s:%d", ip.String(), port)
	})
	if ctlErr != nil {
		return "", ctlErr
	}
	if sockErr != nil {
		return "", sockErr
	}
	return addr, nil
}

// parseSNI extracts the server_name extension from a TLS ClientHello.
// Returns "" if the buffer is too short, malformed, or has no SNI.
//
// We only care about the host string for allowlist matching; we don't
// validate other ClientHello fields, and we tolerate truncation since
// the caller passes a Peek of the first ~1 KiB.
func parseSNI(b []byte) string {
	const recHdr = 5
	const hsHdr = 4
	if len(b) < recHdr+hsHdr || b[0] != 0x16 || b[recHdr] != 0x01 {
		return ""
	}
	p := recHdr + hsHdr + 2 + 32 // version(2) + random(32)
	if len(b) < p+1 {
		return ""
	}
	sidLen := int(b[p])
	p += 1 + sidLen
	if len(b) < p+2 {
		return ""
	}
	csLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2 + csLen
	if len(b) < p+1 {
		return ""
	}
	cmLen := int(b[p])
	p += 1 + cmLen
	if len(b) < p+2 {
		return ""
	}
	extLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	end := p + extLen
	if end > len(b) {
		end = len(b)
	}
	for p+4 <= end {
		extType := binary.BigEndian.Uint16(b[p:])
		extDataLen := int(binary.BigEndian.Uint16(b[p+2:]))
		p += 4
		if extType == 0x0000 { // server_name
			if extDataLen < 5 || p+5 > len(b) {
				return ""
			}
			// server_name_list_length(2) + name_type(1) + name_length(2)
			nameLen := int(binary.BigEndian.Uint16(b[p+3:]))
			if p+5+nameLen > len(b) {
				return ""
			}
			return string(b[p+5 : p+5+nameLen])
		}
		p += extDataLen
	}
	return ""
}

// TLS Alert record fields (RFC 5246 §7.2). We hand-craft the bytes
// instead of standing up a tls.Conn because the client's handshake
// hasn't been processed — we don't have a cipher suite or version
// negotiated, and don't need them: an Alert is the one TLS record a
// peer parses without any prior handshake state.
const (
	tlsAlertFatal        byte = 2
	tlsAlertAccessDenied byte = 49
)

func writeTLSAlert(c net.Conn, level, description byte) {
	_, _ = c.Write([]byte{
		0x15,       // ContentType: alert
		0x03, 0x03, // ProtocolVersion: TLS 1.2 (max compatibility)
		0x00, 0x02, // record length
		level, description,
	})
}

// mirroredTLSHandshake connects to the upstream using the same TLS fingerprint
// the client sent to us. This ensures Cloudflare Bot Management sees the same
// JA3/extension set as the original client (e.g. native-tls/OpenSSL from the
// Rust binary), not Go's default TLS fingerprint. ALPN is pinned to "http/1.1".
func mirroredTLSHandshake(conn net.Conn, serverName string, clientHello []byte) (*utls.UConn, error) {
	var spec utls.ClientHelloSpec
	// allowBluntMimicry=true preserves extensions utls doesn't natively
	// understand (e.g. encrypt_then_mac, ext 22) as raw bytes — required to
	// mirror native-tls/OpenSSL fingerprints faithfully.
	if err := spec.FromRaw(clientHello, true); err != nil {
		return nil, fmt.Errorf("utls mirror spec: %w", err)
	}
	// Pin ALPN to http/1.1; add the extension if the client didn't include it.
	alpnSet := false
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
			spec.Extensions[i] = alpn
			alpnSet = true
			break
		}
	}
	if !alpnSet {
		spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}})
	}
	uconn := utls.UClient(conn, &utls.Config{ServerName: serverName}, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		return nil, fmt.Errorf("utls apply: %w", err)
	}
	if err := uconn.Handshake(); err != nil {
		return nil, err
	}
	return uconn, nil
}

// dialMirroredWithChromeFallback dials origDst and tries to complete a TLS
// handshake that mirrors the client's ClientHello. On any failure — parse,
// apply, or handshake — it redials and retries with the Chrome fingerprint.
// We must redial because a failed handshake leaves the TCP connection in an
// indeterminate state (server bytes may have been written and consumed); a
// fresh socket is the only way to send a clean second ClientHello.
//
// The two-stage attempt is what gets a Rust native-tls client all the way
// through to Cloudflare-protected hosts. Mirroring is preferred because it
// pairs the client's original JA3 with the original HTTP headers; Chrome is
// a much better fallback than stdlib TLS, which is widely fingerprinted as
// bot traffic.
func dialMirroredWithChromeFallback(dialer *net.Dialer, origDst, host string, clientHello []byte) (net.Conn, error) {
	rawUp, err := dialer.DialContext(context.Background(), "tcp", origDst)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	uc, mirrorErr := mirroredTLSHandshake(rawUp, host, clientHello)
	if mirrorErr == nil {
		cs := uc.ConnectionState()
		log.Printf("intercept tls: upstream (mirrored) handshake ok host=%s version=0x%04x cipher=0x%04x", host, cs.Version, cs.CipherSuite)
		return uc, nil
	}
	log.Printf("intercept tls: upstream (mirrored) failed host=%s err=%v — falling back to chrome", host, mirrorErr)
	_ = rawUp.Close()

	rawUp2, err := dialer.DialContext(context.Background(), "tcp", origDst)
	if err != nil {
		return nil, fmt.Errorf("redial for chrome fallback: %w", err)
	}
	uc2, chromeErr := chromeTLSHandshake(rawUp2, host)
	if chromeErr != nil {
		_ = rawUp2.Close()
		return nil, fmt.Errorf("mirror+chrome both failed: mirror=%v chrome=%v", mirrorErr, chromeErr)
	}
	cs := uc2.ConnectionState()
	log.Printf("intercept tls: upstream (chrome fallback) handshake ok host=%s version=0x%04x cipher=0x%04x", host, cs.Version, cs.CipherSuite)
	return uc2, nil
}

// chromeTLSHandshake connects to the upstream using a Chrome ClientHello
// fingerprint so Cloudflare-protected hosts (e.g. chatgpt.com) don't
// block the proxy via JA3 detection. ALPN is pinned to "http/1.1" so
// the HTTP/1.1 inspection loop can read the inner request after the handshake.
func chromeTLSHandshake(conn net.Conn, serverName string) (*utls.UConn, error) {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		return nil, fmt.Errorf("utls spec: %w", err)
	}
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
			spec.Extensions[i] = alpn
			break
		}
	}
	uconn := utls.UClient(conn, &utls.Config{ServerName: serverName}, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		return nil, fmt.Errorf("utls apply: %w", err)
	}
	if err := uconn.Handshake(); err != nil {
		return nil, err
	}
	return uconn, nil
}

// suppress unused-import warning on platforms that don't pull in io.
var _ = io.EOF
