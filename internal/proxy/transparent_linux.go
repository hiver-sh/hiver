//go:build linux

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
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
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: "TLS",
			Host: origDst, Verdict: "deny",
			Reason: "TLS not yet supported in transparent mode",
		})
	default:
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: "?",
			Host: origDst, Verdict: "deny",
			Reason: "unknown protocol",
		})
	}
}

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
	hostOnly := hostnameOf("", host)

	if !p.allowed(hostOnly) {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: req.Method,
			Host: hostOnly, Path: req.URL.Path, Verdict: "deny",
			Status: http.StatusForbidden, Reason: "not in allowlist",
		})
		_, _ = c.Write([]byte("HTTP/1.1 403 Forbidden\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}

	for _, h := range p.stripHeaders {
		req.Header.Del(h)
	}
	// http.ReadRequest leaves req.RequestURI set; req.Write picks origin
	// form regardless, but clear it so it doesn't accidentally end up as
	// proxy-form on the wire.
	req.RequestURI = ""

	upstream, err := p.dialer.DialContext(req.Context(), "tcp", origDst)
	if err != nil {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: req.Method,
			Host: hostOnly, Path: req.URL.Path, Verdict: "error",
			Reason: err.Error(),
		})
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer upstream.Close()

	if err := req.Write(upstream); err != nil {
		return
	}
	upstreamBR := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamBR, req)
	if err != nil {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: req.Method,
			Host: hostOnly, Path: req.URL.Path, Verdict: "error",
			Reason: err.Error(),
		})
		return
	}
	p.audit(AuditEvent{
		At: time.Now(), Type: "network", Method: req.Method,
		Host: hostOnly, Path: req.URL.Path, Verdict: "allow",
		Status: resp.StatusCode,
	})
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

// suppress unused-import warning on platforms that don't pull in io.
var _ = io.EOF
