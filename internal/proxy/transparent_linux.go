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
	"net"
	"net/http"
	"syscall"
	"unsafe"
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
		p.beginAudit("?", origDst, "").deny("unknown protocol", 0)
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
	hello, _ := peekTLSRecord(br)
	host := parseSNI(hello)
	if host == "" {
		host = origDst
	}
	_, port := splitHostPort("", origDst, 0)
	rule := MatchEgress(p.currentAllow(), "TLS", host, port, "")
	if rule == nil {
		p.beginAudit("TLS", host, "").deny("no matching rule", 0)
		// Send a fatal TLS Alert so the peer surfaces a concrete error
		// ("tlsv1 alert access denied") instead of the bare connection
		// close it would otherwise see as `SSL_ERROR_SYSCALL`.
		writeTLSAlert(c, tlsAlertFatal, tlsAlertAccessDenied)
		return
	}
	if p.minter != nil {
		p.interceptTLS(c, br, host, origDst)
		return
	}
	p.rawForwardTLS(c, br, host, origDst)
}

func (p *Proxy) rawForwardTLS(c *net.TCPConn, br *bufio.Reader, host, origDst string) {
	// Raw-forward TLS is host-only: there's no HTTP-level response shape
	// to report, so the decision is the whole audit story. Dial failure
	// before the tunnel exists is treated as a deny so consumers don't
	// see an orphan allow.
	ac := p.beginAudit("TLS", host, "")
	upstream, err := p.dialer.DialContext(context.Background(), "tcp", origDst)
	if err != nil {
		ac.deny("upstream dial: "+err.Error(), 0)
		return
	}
	defer upstream.Close()
	ac.allow()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, upstream); done <- struct{}{} }()
	<-done
}

// interceptTLS terminates TLS, applies HTTP-level rules to the inner
// request, and proxies it to the upstream over a separate TLS
// connection. One request per connection (Connection: close) — the
// HTTP/1.1 keep-alive loop is a follow-up.
func (p *Proxy) interceptTLS(c *net.TCPConn, br *bufio.Reader, host, origDst string) {
	innerCfg := &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.minter.Mint(host)
		},
		// Force HTTP/1.1 — our inspection loop reads request lines,
		// not HPACK frames. Modern hosts negotiate h2 via ALPN; pinning
		// http/1.1 keeps the agent's stack on the version we can read.
		NextProtos: []string{"http/1.1"},
	}
	// Handshake/dial failures here happen before we know the inner HTTP
	// request, so the audit event is host-only (method "TLS"). The inner
	// allowlist match — once we can read req.Method / req.URL.Path —
	// opens a fresh auditCtx with the proper request/response pair.
	tlsAC := p.beginAudit("TLS", host, "")
	clientTLS := tls.Server(&peekedConn{Conn: c, r: br}, innerCfg)
	if err := clientTLS.HandshakeContext(context.Background()); err != nil {
		tlsAC.deny("inner handshake: "+err.Error(), 0)
		return
	}
	defer clientTLS.Close()

	rawUp, err := p.dialer.DialContext(context.Background(), "tcp", origDst)
	if err != nil {
		tlsAC.deny("upstream dial: "+err.Error(), 0)
		return
	}
	upstreamTLS := tls.Client(rawUp, &tls.Config{
		ServerName: host,
		NextProtos: []string{"http/1.1"},
	})
	if err := upstreamTLS.HandshakeContext(context.Background()); err != nil {
		tlsAC.deny("upstream handshake: "+err.Error(), 0)
		_ = rawUp.Close()
		return
	}
	defer upstreamTLS.Close()

	req, err := http.ReadRequest(bufio.NewReader(clientTLS))
	if err != nil {
		return
	}
	ac := p.beginAudit(req.Method, host, req.URL.Path)
	_, port := splitHostPort("", origDst, 0)
	rule := MatchEgress(p.currentAllow(), req.Method, host, port, req.URL.Path)
	if rule == nil {
		ac.deny("no matching rule", http.StatusForbidden)
		_, _ = clientTLS.Write([]byte("HTTP/1.1 403 Forbidden\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	ac.allow()
	for _, h := range p.stripHeaders {
		req.Header.Del(h)
	}
	for k, v := range rule.Headers {
		req.Header.Set(k, v)
	}
	req.RequestURI = ""
	req.Header.Set("Connection", "close")

	if err := req.Write(upstreamTLS); err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(upstreamTLS), req)
	if err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ac.response(resp.StatusCode)
	_ = resp.Write(clientTLS)
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

	ac := p.beginAudit(req.Method, hostOnly, req.URL.Path)
	rule := MatchEgress(p.currentAllow(), req.Method, hostOnly, port, req.URL.Path)
	if rule == nil {
		ac.deny("no matching rule", http.StatusForbidden)
		writeDenyHTTP(c, hostOnly)
		return
	}
	ac.allow()

	for _, h := range p.stripHeaders {
		req.Header.Del(h)
	}
	for k, v := range rule.Headers {
		req.Header.Set(k, v)
	}
	// http.ReadRequest leaves req.RequestURI set; req.Write picks origin
	// form regardless, but clear it so it doesn't accidentally end up as
	// proxy-form on the wire.
	req.RequestURI = ""

	upstream, err := p.dialer.DialContext(req.Context(), "tcp", origDst)
	if err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer upstream.Close()

	if err := req.Write(upstream); err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		return
	}
	upstreamBR := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamBR, req)
	if err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		return
	}
	ac.response(resp.StatusCode)
	_ = resp.Write(c)
	_ = resp.Body.Close()
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

// suppress unused-import warning on platforms that don't pull in io.
var _ = io.EOF
