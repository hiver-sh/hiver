//go:build linux

package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newForwardTestProxy() *Proxy {
	return &Proxy{auditEnc: json.NewEncoder(io.Discard), upstreamPool: newUpstreamPool()}
}

// fakeUpstream serves one HTTP/1.1 response over a net.Pipe: it reads the request
// the proxy writes, then replies with rawResp. Returns the proxy-side conn.
func fakeUpstream(t *testing.T, rawResp string) net.Conn {
	t.Helper()
	proxySide, originSide := net.Pipe()
	go func() {
		defer originSide.Close()
		if _, err := http.ReadRequest(bufio.NewReader(originSide)); err != nil {
			return
		}
		io.WriteString(originSide, rawResp)
	}()
	return proxySide
}

// TestForwardH2KeepAliveReusable: a clean keep-alive response is streamed to the
// h2 writer and the upstream conn is reported reusable (so it can be pooled).
func TestForwardH2KeepAliveReusable(t *testing.T) {
	upstream := fakeUpstream(t, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 5\r\n\r\nhello")
	defer upstream.Close()

	p := newForwardTestProxy()
	r := httptest.NewRequest("GET", "https://example.com/", nil)
	r.RequestURI = ""
	w := httptest.NewRecorder()
	ac := p.beginAudit("10.0.0.1", "GET", "example.com", "/", "")

	reusable := p.forwardH2Upstream(w, r, upstream, bufio.NewReader(upstream), ac)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "hello" {
		t.Fatalf("body = %q, want %q", w.Body.String(), "hello")
	}
	if !reusable {
		t.Fatal("keep-alive response should leave the upstream conn reusable")
	}
}

// TestForwardH2ConnectionCloseNotReusable: an upstream that says "Connection:
// close" must not be pooled (still delivers the body).
func TestForwardH2ConnectionCloseNotReusable(t *testing.T) {
	upstream := fakeUpstream(t, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 3\r\nConnection: close\r\n\r\nbye")
	defer upstream.Close()

	p := newForwardTestProxy()
	r := httptest.NewRequest("GET", "https://example.com/", nil)
	r.RequestURI = ""
	w := httptest.NewRecorder()
	ac := p.beginAudit("10.0.0.1", "GET", "example.com", "/", "")

	reusable := p.forwardH2Upstream(w, r, upstream, bufio.NewReader(upstream), ac)

	if w.Body.String() != "bye" {
		t.Fatalf("body = %q, want %q", w.Body.String(), "bye")
	}
	if reusable {
		t.Fatal("Connection: close response must not be reused")
	}
}

// TestCopyResponseHeadersStripsHopByHop: hop-by-hop headers illegal in h2 are
// dropped; end-to-end headers pass through.
func TestCopyResponseHeadersStripsHopByHop(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "text/html")
	src.Set("Connection", "keep-alive")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Upgrade", "h2c")
	src.Set("X-Custom", "v")

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	if dst.Get("Content-Type") != "text/html" || dst.Get("X-Custom") != "v" {
		t.Fatal("end-to-end headers should pass through")
	}
	for _, h := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade"} {
		if dst.Get(h) != "" {
			t.Fatalf("hop-by-hop header %q should be stripped", h)
		}
	}
}
