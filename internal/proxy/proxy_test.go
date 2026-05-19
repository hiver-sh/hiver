package proxy_test

import (
	"bytes"
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
)

// startProxy boots a proxy on a random port and returns the HTTP client
// configured to use it, plus the audit buffer for assertions.
func startProxy(t *testing.T, rules []proxy.EgressRule) (*http.Client, *bytes.Buffer, func()) {
	t.Helper()
	audit := &bytes.Buffer{}
	p, err := proxy.New(proxy.Config{Addr: "127.0.0.1:0", Allow: rules, Audit: audit})
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
		// Confirm the auth header was stripped before forwarding.
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("expected Authorization stripped; got %q", got)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	upstreamHost, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	client, audit, stop := startProxy(t, []proxy.EgressRule{{Host: upstreamHost}})
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
	if len(events) != 1 {
		t.Fatalf("audit events: got %d, want 1 (deny is request-only): %+v", len(events), events)
	}
	if events[0].Phase != "request" || events[0].Verdict != "deny" {
		t.Errorf("expected request-deny event; got %+v", events[0])
	}
}

func TestMatchEgress(t *testing.T) {
	rules := []proxy.EgressRule{
		{Host: "api.github.com", Methods: []string{"GET"}, Paths: []string{"/repos/*"}},
		{Host: "*.pypi.org"}, // any method, any path, any port
		{Host: "files.example.com", Override: &proxy.EgressOverride{Headers: map[string]string{"X-Auth": "tok"}}},
		{Host: "127.0.0.1", Ports: []int{8080}},                    // port-pinned, host-only
		{Host: "metrics.internal", Ports: []int{9090, 9091, 9092}}, // port set
	}
	cases := []struct {
		name             string
		method, host     string
		port             int
		p                string
		wantMatch        bool
		wantHeaderInject string
	}{
		{"http: full match on rule 1", "GET", "api.github.com", 443, "/repos/foo", true, ""},
		{"http: method denied", "POST", "api.github.com", 443, "/repos/foo", false, ""},
		{"http: path denied", "GET", "api.github.com", 443, "/users/foo", false, ""},
		{"http: wildcard host any method/path", "POST", "files.pypi.org", 443, "/anything", true, ""},
		{"http: wildcard apex excluded", "GET", "pypi.org", 443, "/", false, ""},
		{"http: header rule injects", "GET", "files.example.com", 80, "/x", true, "tok"},
		{"tls: host-only match (path empty)", "TLS", "files.pypi.org", 443, "", true, ""},
		{"tls: host miss", "TLS", "evil.com", 443, "", false, ""},
		{"port: pinned port matches", "GET", "127.0.0.1", 8080, "/x", true, ""},
		{"port: pinned port wrong port denied", "GET", "127.0.0.1", 22, "/x", false, ""},
		{"port: list match", "GET", "metrics.internal", 9091, "/m", true, ""},
		{"port: list miss", "GET", "metrics.internal", 9100, "/m", false, ""},
		{"port: unenforced rule ignores port", "GET", "files.pypi.org", 8443, "/", true, ""},
		{"empty host always denied", "GET", "", 80, "/", false, ""},
		{"empty rules deny everything", "GET", "anywhere.com", 80, "/", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := rules
			if tc.name == "empty rules deny everything" {
				r = nil
			}
			got := proxy.MatchEgress(r, tc.method, tc.host, tc.port, tc.p)
			if (got != nil) != tc.wantMatch {
				t.Fatalf("MatchEgress(%q,%q,%d,%q) match=%v, want %v", tc.method, tc.host, tc.port, tc.p, got != nil, tc.wantMatch)
			}
			if tc.wantHeaderInject != "" {
				if got == nil || got.Override == nil || got.Override.Headers["X-Auth"] != tc.wantHeaderInject {
					t.Errorf("expected header X-Auth=%q on matched rule, got %+v", tc.wantHeaderInject, got)
				}
			}
		})
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
	client, _, stop := startProxy(t, []proxy.EgressRule{{Host: upstreamHost}})
	defer stop()

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
