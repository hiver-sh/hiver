package proxy_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/blasten/hive/internal/proxy"
	"github.com/klauspost/compress/zstd"
)

// startProxy boots a proxy on a random port and returns the HTTP client
// configured to use it, plus the audit buffer for assertions.
func startProxy(t *testing.T, rules []proxy.EgressRule) (*http.Client, *bytes.Buffer, func()) {
	t.Helper()
	audit := &bytes.Buffer{}
	p, err := proxy.New(proxy.Config{Addr: "127.0.0.1:0", Rules: rules, Audit: audit})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	if err := p.Listen(); err != nil {
		t.Fatalf("proxy.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := p.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("proxy.Run: %v", err)
		}
	}()

	proxyURL, _ := url.Parse("http://" + p.Addr())
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	return client, audit, func() {
		cancel()
		// Give Shutdown a moment.
		time.Sleep(50 * time.Millisecond)
	}
}

func decodeAudit(t *testing.T, b *bytes.Buffer) []proxy.AuditEvent {
	t.Helper()
	var out []proxy.AuditEvent
	dec := json.NewDecoder(b)
	for {
		var e proxy.AuditEvent
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode audit: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestHTTPAllowedForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auth header passes through — no stripping by default.
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	client, audit, stop := startProxy(t, []proxy.EgressRule{{Access: "allow", Host: upstreamHost}})
	defer stop()

	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer secret-from-agent")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Errorf("body: got %q, want %q", body, "hello")
	}

	events := decodeAudit(t, audit)
	// request + response (now emitted at headers) + stream_chunk per body Read
	if len(events) < 3 {
		t.Fatalf("audit events: got %d, want >=3 (request+response+>=1 stream_chunk): %+v", len(events), events)
	}
	if events[0].Phase != "request" || events[0].Verdict != "allow" || events[0].Method != "GET" {
		t.Errorf("request event mismatch: %+v", events[0])
	}
	if events[1].Phase != "response" || events[1].Verdict != "allow" || events[1].Status != 200 {
		t.Errorf("response event mismatch: %+v", events[1])
	}
	// Body should come as one or more stream_chunk events after the response.
	var chunkBody string
	for _, e := range events[2:] {
		if e.Phase == "stream_chunk" {
			chunkBody += e.Body
		}
	}
	if chunkBody != "hello" {
		t.Errorf("stream_chunk body: got %q, want %q", chunkBody, "hello")
	}
	for _, e := range events {
		if e.RequestID != events[0].RequestID {
			t.Errorf("all events should share RequestID; got %d vs %d in %+v", e.RequestID, events[0].RequestID, e)
		}
	}
}

func TestHTTPDeniedReturns403(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should never be reached")
		w.WriteHeader(500)
	}))
	defer upstream.Close()

	// Allowlist is empty — all egress denied.
	client, audit, stop := startProxy(t, nil)
	defer stop()

	resp, err := client.Get(upstream.URL + "/secret")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", resp.StatusCode)
	}

	events := decodeAudit(t, audit)
	if len(events) != 2 {
		t.Fatalf("audit events: got %d, want 2 (deny request+response): %+v", len(events), events)
	}
	if events[0].Phase != "request" || events[0].Verdict != "deny" {
		t.Errorf("expected request-deny event; got %+v", events[0])
	}
	if events[1].Phase != "response" || events[1].Verdict != "deny" {
		t.Errorf("expected response-deny event; got %+v", events[1])
	}
}

func TestMatchEgress(t *testing.T) {
	rules := []proxy.EgressRule{
		{Access: "allow", Host: "api.github.com", Methods: []string{"GET"}, Paths: []string{"/repos/*"}},
		{Access: "allow", Host: "*.pypi.org"}, // any method, any path, any port
		{Access: "allow", Host: "files.example.com", Override: &proxy.EgressOverride{Headers: map[string]string{"X-Auth": "tok"}}},
		{Access: "allow", Host: "127.0.0.1", Ports: []int{8080}},                    // port-pinned, host-only
		{Access: "allow", Host: "metrics.internal", Ports: []int{9090, 9091, 9092}}, // port set
		{Access: "deny", Host: "blocked.com"},                                       // explicit deny rule
	}
	cases := []struct {
		name             string
		method, host     string
		port             int
		p                string
		wantAccess       string // "allow", "deny", or "" for no match
		wantHeaderInject string
	}{
		{"http: full match on rule 1", "GET", "api.github.com", 443, "/repos/foo", "allow", ""},
		{"http: method miss", "POST", "api.github.com", 443, "/repos/foo", "", ""},
		{"http: path miss", "GET", "api.github.com", 443, "/users/foo", "", ""},
		{"http: wildcard host any method/path", "POST", "files.pypi.org", 443, "/anything", "allow", ""},
		{"http: wildcard apex excluded", "GET", "pypi.org", 443, "/", "", ""},
		{"http: header rule injects", "GET", "files.example.com", 80, "/x", "allow", "tok"},
		{"tls: host-only match (path empty)", "TLS", "files.pypi.org", 443, "", "allow", ""},
		{"tls: host miss", "TLS", "evil.com", 443, "", "", ""},
		{"port: pinned port matches", "GET", "127.0.0.1", 8080, "/x", "allow", ""},
		{"port: pinned port wrong port denied", "GET", "127.0.0.1", 22, "/x", "", ""},
		{"port: list match", "GET", "metrics.internal", 9091, "/m", "allow", ""},
		{"port: list miss", "GET", "metrics.internal", 9100, "/m", "", ""},
		{"port: unenforced rule ignores port", "GET", "files.pypi.org", 8443, "/", "allow", ""},
		{"deny rule matches", "GET", "blocked.com", 443, "/", "deny", ""},
		{"empty host always denied", "GET", "", 80, "/", "", ""},
		{"empty rules deny everything", "GET", "anywhere.com", 80, "/", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := rules
			if tc.name == "empty rules deny everything" {
				r = nil
			}
			got := proxy.MatchEgress(r, tc.method, tc.host, tc.port, tc.p)
			if tc.wantAccess == "" {
				if got != nil {
					t.Fatalf("MatchEgress(%q,%q,%d,%q) got match access=%q, want no match", tc.method, tc.host, tc.port, tc.p, got.Access)
				}
				return
			}
			if got == nil {
				t.Fatalf("MatchEgress(%q,%q,%d,%q) got no match, want access=%q", tc.method, tc.host, tc.port, tc.p, tc.wantAccess)
			}
			if got.Access != tc.wantAccess {
				t.Errorf("got access=%q, want %q", got.Access, tc.wantAccess)
			}
			if tc.wantHeaderInject != "" {
				if got.Override == nil || got.Override.Headers["X-Auth"] != tc.wantHeaderInject {
					t.Errorf("expected header X-Auth=%q on matched rule, got %+v", tc.wantHeaderInject, got)
				}
			}
		})
	}
}

// TestPathDenyDoesNotBlockTLSTunnel verifies that a deny rule with a path
// restriction is skipped at the TLS/CONNECT level (where path is unknown)
// and is only enforced at the HTTP request level. This ensures that
// deny /hasown doesn't kill the entire HTTPS tunnel to the host.
func TestPathDenyDoesNotBlockTLSTunnel(t *testing.T) {
	rules := []proxy.EgressRule{
		{Access: "deny", Host: "registry.npmjs.org", Paths: []string{"/hasown"}},
		{Access: "allow", Host: "registry.npmjs.org", Paths: []string{"/*"}},
	}

	// TLS tunnel (path=""): deny rule must be skipped; allow /* matches.
	tls := proxy.MatchEgress(rules, "TLS", "registry.npmjs.org", 443, "")
	if tls == nil || tls.Access != "allow" {
		t.Fatalf("TLS tunnel: got %v, want allow (deny /hasown must not block tunnel)", tls)
	}

	// HTTP /hasown: deny fires.
	hasown := proxy.MatchEgress(rules, "GET", "registry.npmjs.org", 443, "/hasown")
	if hasown == nil || hasown.Access != "deny" {
		t.Fatalf("/hasown: got %v, want deny", hasown)
	}

	// HTTP /express: allow /* fires.
	express := proxy.MatchEgress(rules, "GET", "registry.npmjs.org", 443, "/express")
	if express == nil || express.Access != "allow" {
		t.Fatalf("/express: got %v, want allow", express)
	}

	// Host-only deny (no paths) must still block the tunnel.
	hostDenyRules := []proxy.EgressRule{
		{Access: "deny", Host: "blocked.com"},
	}
	tunnel := proxy.MatchEgress(hostDenyRules, "TLS", "blocked.com", 443, "")
	if tunnel == nil || tunnel.Access != "deny" {
		t.Fatalf("host-only deny: got %v, want deny", tunnel)
	}
}

func TestAuthHeadersPassThrough(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(204)
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	client, _, stop := startProxy(t, []proxy.EgressRule{{Access: "allow", Host: upstreamHost}})
	defer stop()

	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Cookie", "session=xyz")
	req.Header.Set("X-Api-Key", "hunter2")
	resp, _ := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}

	for _, h := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if got := seen.Get(h); got == "" {
			t.Errorf("expected %s forwarded to upstream, got empty", h)
		}
	}
}

func TestRequestHeadersInAudit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response-Id", "resp-42")
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	client, audit, stop := startProxy(t, []proxy.EgressRule{{Access: "allow", Host: upstreamHost}})
	defer stop()

	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/data", nil)
	req.Header.Set("X-Request-Tag", "tag-99")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	events := decodeAudit(t, audit)
	if len(events) != 2 {
		t.Fatalf("want 2 audit events, got %d", len(events))
	}
	reqEv := events[0]
	if reqEv.Phase != "request" {
		t.Fatalf("events[0] phase=%q, want request", reqEv.Phase)
	}
	if reqEv.Headers == nil {
		t.Fatal("request event: Headers is nil, want non-nil for HTTP flows")
	}
	if got := reqEv.Headers["X-Request-Tag"]; got != "tag-99" {
		t.Errorf("request headers: X-Request-Tag=%q, want %q", got, "tag-99")
	}

	resEv := events[1]
	if resEv.Phase != "response" {
		t.Fatalf("events[1] phase=%q, want response", resEv.Phase)
	}
	if resEv.Headers == nil {
		t.Fatal("response event: Headers is nil, want non-nil for HTTP flows")
	}
	if got := resEv.Headers["X-Response-Id"]; got != "resp-42" {
		t.Errorf("response headers: X-Response-Id=%q, want %q", got, "resp-42")
	}
}

func TestAuthHeadersInAudit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	client, audit, stop := startProxy(t, []proxy.EgressRule{{Access: "allow", Host: upstreamHost}})
	defer stop()

	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Safe-Header", "visible")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	events := decodeAudit(t, audit)
	if len(events) < 1 {
		t.Fatal("no audit events")
	}
	reqEv := events[0]
	if reqEv.Headers == nil {
		t.Fatal("request event: Headers is nil")
	}
	if got := reqEv.Headers["Authorization"]; got != "Bearer secret" {
		t.Errorf("Authorization=%q in audit, want %q", got, "Bearer secret")
	}
	if got := reqEv.Headers["X-Safe-Header"]; got != "visible" {
		t.Errorf("X-Safe-Header=%q, want %q", got, "visible")
	}
}

func TestGzipResponseDecompressed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte("hello gzip"))
		_ = gz.Close()
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	client, _, stop := startProxy(t, []proxy.EgressRule{{Access: "allow", Host: upstreamHost}})
	defer stop()

	resp, err := client.Get(upstream.URL + "/")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello gzip" {
		t.Errorf("body: got %q, want %q", body, "hello gzip")
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding should be stripped, got %q", resp.Header.Get("Content-Encoding"))
	}
}

func TestZstdResponseDecompressed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "zstd")
		w.Header().Set("Content-Type", "text/plain")
		enc, _ := zstd.NewWriter(w)
		_, _ = enc.Write([]byte("hello zstd"))
		_ = enc.Close()
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	client, _, stop := startProxy(t, []proxy.EgressRule{{Access: "allow", Host: upstreamHost}})
	defer stop()

	resp, err := client.Get(upstream.URL + "/")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello zstd" {
		t.Errorf("body: got %q, want %q", body, "hello zstd")
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding should be stripped, got %q", resp.Header.Get("Content-Encoding"))
	}
}

func TestZstdRequestBodyDecodedInAudit(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b) // upstream sees raw compressed bytes (Content-Encoding still set)
		w.WriteHeader(204)
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	client, audit, stop := startProxy(t, []proxy.EgressRule{{Access: "allow", Host: upstreamHost}})
	defer stop()

	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf)
	_, _ = enc.Write([]byte("hello zstd request"))
	_ = enc.Close()

	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/", &buf)
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	// Audit must contain plain-text body.
	events := decodeAudit(t, audit)
	var reqEv proxy.AuditEvent
	for _, e := range events {
		if e.Phase == "request" {
			reqEv = e
			break
		}
	}
	if reqEv.Body != "hello zstd request" {
		t.Errorf("audit request body: got %q, want %q", reqEv.Body, "hello zstd request")
	}

	// Upstream must still receive the compressed bytes (proxy does not strip Content-Encoding).
	if gotBody == "hello zstd request" {
		t.Error("upstream should receive compressed bytes, not plain text")
	}
}

// wsMakeFrame builds a WebSocket text frame. Client frames are masked
// (RFC 6455 §5.3); server frames are not.
func wsMakeFrame(opcode byte, fromClient bool, payload []byte) []byte {
	frame := []byte{0x80 | opcode} // FIN=1
	pl := len(payload)
	if fromClient {
		frame = append(frame, 0x80|byte(pl))
		key := [4]byte{0x37, 0xFA, 0x21, 0x3D}
		frame = append(frame, key[:]...)
		for i, b := range payload {
			frame = append(frame, b^key[i%4])
		}
	} else {
		frame = append(frame, byte(pl))
		frame = append(frame, payload...)
	}
	return frame
}

// wsReadFrameTest reads one WebSocket frame (payload < 126 bytes) and
// returns the unmasked payload. Used only in tests.
func wsReadFrameTest(r io.Reader) (opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return
	}
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	pl := int(hdr[1] & 0x7F)
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return
		}
	}
	payload = make([]byte, pl)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return
}

func TestWebSocketProxied(t *testing.T) {
	// Upstream: respond 101, then echo WebSocket frames properly (parse masked
	// client frames, send back unmasked server frames).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected ws upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		br := bufio.NewReader(conn)
		for {
			opcode, payload, err := wsReadFrameTest(br)
			if err != nil || opcode == 0x08 {
				return
			}
			if _, err := conn.Write(wsMakeFrame(opcode, false, payload)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))

	audit := &bytes.Buffer{}
	p, err := proxy.New(proxy.Config{Addr: "127.0.0.1:0", Rules: []proxy.EgressRule{{Access: "allow", Host: upstreamHost}}, Audit: audit})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	if err := p.Listen(); err != nil {
		t.Fatalf("proxy.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx) //nolint:errcheck

	// Dial the proxy directly and send a WebSocket upgrade in proxy-request form.
	conn, err := net.DialTimeout("tcp", p.Addr(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}

	_, _ = fmt.Fprintf(conn,
		"GET %s/ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
		upstream.URL, upstreamHost,
	)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}

	// Send a masked text frame, read back the unmasked echo.
	msg := []byte("hello ws")
	if _, err := conn.Write(wsMakeFrame(0x1, true, msg)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	opcode, got, err := wsReadFrameTest(br)
	if err != nil {
		t.Fatalf("read echo frame: %v", err)
	}
	if opcode != 0x1 || string(got) != string(msg) {
		t.Errorf("echo: opcode=%d payload=%q, want opcode=1 payload=%q", opcode, got, msg)
	}

	// Close the connection so the proxy tunnel goroutines exit and emit all
	// audit events before we read the buffer.
	conn.Close()
	time.Sleep(50 * time.Millisecond)

	events := decodeAudit(t, audit)
	// expect: request + stream_chunk (client→upstream) + stream_chunk (upstream→client) + response
	if len(events) != 4 {
		t.Fatalf("audit events: got %d, want 4 (request+2×stream_chunk+response): %+v", len(events), events)
	}
	var reqEv, respEv proxy.AuditEvent
	var chunks []proxy.AuditEvent
	for _, e := range events {
		switch e.Phase {
		case "request":
			reqEv = e
		case "response":
			respEv = e
		case "stream_chunk":
			chunks = append(chunks, e)
		}
	}
	if reqEv.Verdict != "allow" || reqEv.Method != "GET" {
		t.Errorf("request event: %+v", reqEv)
	}
	if respEv.Verdict != "allow" || respEv.Status != http.StatusSwitchingProtocols {
		t.Errorf("response event: %+v", respEv)
	}
	if len(chunks) != 2 {
		t.Fatalf("stream_chunk events: got %d, want 2: %+v", len(chunks), chunks)
	}
	// Both chunks carry the raw message payload; the direction lives
	// on the Label field ("up" for client→upstream, "down" for the
	// echo back). Order is non-deterministic; assert the set.
	labels := map[string]string{}
	for _, c := range chunks {
		if c.Body != string(msg) {
			t.Errorf("stream_chunk body: got %q, want %q", c.Body, msg)
		}
		labels[c.Label] = c.Body
	}
	if _, ok := labels["up"]; !ok {
		t.Errorf("missing stream_chunk with label=up; labels=%+v", labels)
	}
	if _, ok := labels["down"]; !ok {
		t.Errorf("missing stream_chunk with label=down; labels=%+v", labels)
	}
}

func TestWebSocketDenied(t *testing.T) {
	audit := &bytes.Buffer{}
	p, err := proxy.New(proxy.Config{Addr: "127.0.0.1:0", Rules: nil, Audit: audit})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	if err := p.Listen(); err != nil {
		t.Fatalf("proxy.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx) //nolint:errcheck

	conn, err := net.DialTimeout("tcp", p.Addr(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	_, _ = fmt.Fprint(conn,
		"GET http://example.com/ws HTTP/1.1\r\nHost: example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
	)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}

	events := decodeAudit(t, audit)
	if len(events) != 2 {
		t.Fatalf("audit events: got %d, want 2: %+v", len(events), events)
	}
	if events[0].Verdict != "deny" || events[1].Verdict != "deny" {
		t.Errorf("expected both events to be deny: %+v", events)
	}
}

func TestMatchEgressWildcardHost(t *testing.T) {
	// bare "*" allows everything
	rules := []proxy.EgressRule{{Access: "allow", Host: "*"}}
	for _, host := range []string{"api.anthropic.com", "anything.example.com", "192.0.2.1"} {
		if got := proxy.MatchEgress(rules, "GET", host, 443, "/"); got == nil || got.Access != "allow" {
			t.Errorf("bare *: host %q got %v, want allow", host, got)
		}
	}
	if got := proxy.MatchEgress(rules, "GET", "", 80, "/"); got != nil {
		t.Errorf("empty host: got match %v, want nil", got)
	}

	// glob patterns
	cases := []struct {
		pat        string
		host       string
		wantMatch  bool
	}{
		// *.suffix — matches subdomain but not apex
		{"*.host", "foo.host", true},
		{"*.host", "bar.baz.host", true},
		{"*.host", "host", false},
		// *.mid.* — wildcard on both sides
		{"*.host.*", "foo.host.bar", true},
		{"*.host.*", "foo.host.bar.baz", true},
		{"*.host.*", "foo.host", false},  // no trailing segment
		{"*.host.*", "host.bar", false},  // no leading segment
		// exact (no wildcard)
		{"exact.com", "exact.com", true},
		{"exact.com", "other.com", false},
	}
	for _, tc := range cases {
		r := []proxy.EgressRule{{Access: "allow", Host: tc.pat}}
		got := proxy.MatchEgress(r, "GET", tc.host, 443, "/path")
		matched := got != nil && got.Access == "allow"
		if matched != tc.wantMatch {
			t.Errorf("pat=%q host=%q: got match=%v, want %v", tc.pat, tc.host, matched, tc.wantMatch)
		}
	}
}
