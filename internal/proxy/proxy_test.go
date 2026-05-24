package proxy_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
	if len(events) != 2 {
		t.Fatalf("audit events: got %d, want 2 (request+response): %+v", len(events), events)
	}
	if events[0].Phase != "request" || events[0].Verdict != "allow" || events[0].Method != "GET" {
		t.Errorf("request event mismatch: %+v", events[0])
	}
	if events[1].Phase != "response" || events[1].Verdict != "allow" || events[1].Status != 200 {
		t.Errorf("response event mismatch: %+v", events[1])
	}
	if events[0].RequestID == 0 || events[0].RequestID != events[1].RequestID {
		t.Errorf("request_id should pair the two events: req=%d resp=%d", events[0].RequestID, events[1].RequestID)
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

func TestStripDefaultAuthHeaders(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(204)
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	// Explicitly opt into the default auth-header strip list.
	audit := &bytes.Buffer{}
	p, err := proxy.New(proxy.Config{
		Addr:  "127.0.0.1:0",
		Rules: []proxy.EgressRule{{Access: "allow", Host: upstreamHost}},
		Audit: audit,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	if err := p.Listen(); err != nil {
		t.Fatalf("proxy.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx) //nolint:errcheck
	addr := p.Addr()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: addr})}}
	defer cancel()

	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer leaked")
	req.Header.Set("Cookie", "session=xyz")
	req.Header.Set("X-Api-Key", "hunter2")
	req.Header.Set("X-Trace-Id", "keep-me") // not in strip list
	resp, _ := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}

	for _, h := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if got := seen.Get(h); got != "" {
			t.Errorf("expected %s stripped, got %q", h, got)
		}
	}
	if got := seen.Get("X-Trace-Id"); got != "keep-me" {
		t.Errorf("expected X-Trace-Id preserved, got %q", got)
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

func TestStrippedHeadersAbsentFromAudit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	audit := &bytes.Buffer{}
	p, err := proxy.New(proxy.Config{
		Addr:  "127.0.0.1:0",
		Rules: []proxy.EgressRule{{Access: "allow", Host: upstreamHost}},
		Audit: audit,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	if err := p.Listen(); err != nil {
		t.Fatalf("proxy.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx) //nolint:errcheck
	defer cancel()

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: p.Addr()})}}
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
	if reqEv.Headers != nil {
		if _, hasAuth := reqEv.Headers["Authorization"]; hasAuth {
			t.Error("Authorization header must be absent from audit after stripping")
		}
		if got := reqEv.Headers["X-Safe-Header"]; got != "visible" {
			t.Errorf("X-Safe-Header=%q, want %q", got, "visible")
		}
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
