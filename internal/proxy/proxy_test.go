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

	"github.com/sandbox-platform/agent-sandbox/internal/proxy"
)

// startProxy boots a proxy on a random port and returns the HTTP client
// configured to use it, plus the audit buffer for assertions.
func startProxy(t *testing.T, allow []string) (*http.Client, *bytes.Buffer, func()) {
	t.Helper()
	audit := &bytes.Buffer{}
	p, err := proxy.New(proxy.Config{Addr: "127.0.0.1:0", Allow: allow, Audit: audit})
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
	client, audit, stop := startProxy(t, []string{upstreamHost})
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
	if len(events) != 1 {
		t.Fatalf("audit events: got %d, want 1: %+v", len(events), events)
	}
	if events[0].Verdict != "allow" || events[0].Method != "GET" {
		t.Errorf("audit event mismatch: %+v", events[0])
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
		t.Fatalf("audit events: got %d, want 1", len(events))
	}
	if events[0].Verdict != "deny" {
		t.Errorf("expected deny verdict; got %+v", events[0])
	}
}

func TestMatchAllowlist(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		allow   []string
		want    bool
	}{
		{"exact match", "api.github.com", []string{"api.github.com"}, true},
		{"exact mismatch", "evil.com", []string{"api.github.com"}, false},
		{"wildcard subdomain", "files.pypi.org", []string{"*.pypi.org"}, true},
		{"wildcard apex excluded", "pypi.org", []string{"*.pypi.org"}, false},
		{"wildcard mismatched suffix", "evil.org", []string{"*.pypi.org"}, false},
		{"empty host", "", []string{"anything.com"}, false},
		{"empty allowlist denies all", "anything.com", nil, false},
		{"multiple patterns, second matches", "files.pypi.org", []string{"api.github.com", "*.pypi.org"}, true},
		{"empty pattern strings ignored", "api.github.com", []string{"", "api.github.com", " "}, true},
		{"deep subdomain on exact", "evil.api.github.com", []string{"api.github.com"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxy.MatchAllowlist(tc.host, tc.allow); got != tc.want {
				t.Errorf("MatchAllowlist(%q, %v) = %v, want %v", tc.host, tc.allow, got, tc.want)
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
	client, _, stop := startProxy(t, []string{upstreamHost})
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
