// Package proxy implements the sandbox's MITM egress proxy.
//
// Prototype scope: HTTP forward proxy + CONNECT tunneling for HTTPS, with
// host-pattern allowlist and JSON-line audit logging. No TLS body inspection
// (no per-sandbox CA), no body inspectors, no credential broker — all those
// belong to later tickets (T57, T60–T65).
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config drives a proxy instance. Allow patterns may be exact hosts
// ("api.github.com") or wildcard suffixes ("*.pypi.org").
type Config struct {
	// Listen address (e.g. "127.0.0.1:3128"). Required.
	Addr string
	// Allowed host patterns. Empty list = deny everything.
	Allow []string
	// Audit sink. Each event is encoded as one JSON line.
	// Required; tests pass *bytes.Buffer, prod passes os.Stderr.
	Audit io.Writer
	// Headers to strip from outbound requests before forwarding.
	// Empty = use [DefaultStrippedAuthHeaders].
	StripHeaders []string
	// OutboundMark, when non-zero, sets SO_MARK on every upstream socket
	// the proxy opens. sandboxd uses this in transparent mode so that
	// iptables can match `-m mark --mark <mark>` and skip the REDIRECT
	// rule for proxy-originated traffic, breaking what would otherwise
	// be a redirect loop. Linux only; ignored on other platforms.
	OutboundMark int
}

// DefaultStrippedAuthHeaders is the set the proxy removes when
// inboundAuthHeaders == "strip" (REQ-36).
var DefaultStrippedAuthHeaders = []string{
	"Authorization", "Cookie", "X-Api-Key", "X-Auth-Token", "Proxy-Authorization",
}

// AuditEvent is one record on the audit.network topic (§9.1).
type AuditEvent struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"` // always "network"
	Method  string    `json:"method"`
	Host    string    `json:"host"`
	Path    string    `json:"path,omitempty"`
	Verdict string    `json:"verdict"`        // "allow" | "deny" | "error"
	Status  int       `json:"status,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// Proxy is the running proxy. Construct with [New], drive with [Run].
type Proxy struct {
	cfg          Config
	listener     net.Listener
	stripHeaders []string
	dialer       *net.Dialer
	transport    *http.Transport

	auditMu  sync.Mutex
	auditEnc *json.Encoder
}

// New validates the config and returns a Proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.Addr == "" {
		return nil, errors.New("proxy: Addr required")
	}
	if cfg.Audit == nil {
		return nil, errors.New("proxy: Audit sink required")
	}
	strip := cfg.StripHeaders
	if len(strip) == 0 {
		strip = DefaultStrippedAuthHeaders
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if cfg.OutboundMark != 0 {
		dialer.Control = soMarkControl(cfg.OutboundMark)
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Proxy{
		cfg:          cfg,
		stripHeaders: strip,
		dialer:       dialer,
		transport:    transport,
		auditEnc:     json.NewEncoder(cfg.Audit),
	}, nil
}

// Listen binds the proxy's listener so callers can read [Addr] before
// the server starts handling requests. Useful for tests that want a
// random port.
func (p *Proxy) Listen() error {
	l, err := net.Listen("tcp", p.cfg.Addr)
	if err != nil {
		return fmt.Errorf("proxy: listen %s: %w", p.cfg.Addr, err)
	}
	p.listener = l
	return nil
}

// Addr returns the listener's bound address (call after [Listen]).
func (p *Proxy) Addr() string {
	if p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

// Run serves requests until ctx is cancelled or an unrecoverable error
// occurs. If [Listen] hasn't been called, Run calls it.
func (p *Proxy) Run(ctx context.Context) error {
	if p.listener == nil {
		if err := p.Listen(); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: p}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	err := srv.Serve(p.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ServeHTTP dispatches plain HTTP and CONNECT requests.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostnameOf(r.URL.Host, r.Host)
	if !p.allowed(host) {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: r.Method,
			Host: host, Path: r.URL.Path, Verdict: "deny",
			Status: http.StatusForbidden, Reason: "not in allowlist",
		})
		http.Error(w, "egress denied: "+host, http.StatusForbidden)
		return
	}

	for _, h := range p.stripHeaders {
		r.Header.Del(h)
	}

	// Rebuild the request for forwarding. http.DefaultTransport requires
	// RequestURI to be empty and URL to have a scheme + host.
	r.RequestURI = ""
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}

	out, err := p.transport.RoundTrip(r)
	if err != nil {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: r.Method,
			Host: host, Path: r.URL.Path, Verdict: "error",
			Status: http.StatusBadGateway, Reason: err.Error(),
		})
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer out.Body.Close()

	for k, vs := range out.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(out.StatusCode)
	_, _ = io.Copy(w, out.Body)

	p.audit(AuditEvent{
		At: time.Now(), Type: "network", Method: r.Method,
		Host: host, Path: r.URL.Path, Verdict: "allow", Status: out.StatusCode,
	})
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := hostnameOf("", r.Host)
	if !p.allowed(host) {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: "CONNECT",
			Host: host, Verdict: "deny", Status: http.StatusForbidden,
			Reason: "not in allowlist",
		})
		http.Error(w, "egress denied: "+host, http.StatusForbidden)
		return
	}

	upstream, err := p.dialer.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: "CONNECT",
			Host: host, Verdict: "error", Reason: err.Error(),
		})
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "no hijacker", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}

	p.audit(AuditEvent{
		At: time.Now(), Type: "network", Method: "CONNECT",
		Host: host, Verdict: "allow",
	})

	// Bidi tunnel until either side closes.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = upstream.Close()
}

// allowed checks host against the proxy's allowlist.
func (p *Proxy) allowed(host string) bool {
	return MatchAllowlist(host, p.cfg.Allow)
}

// MatchAllowlist reports whether host matches any pattern in allow.
// Patterns are exact hostnames ("api.github.com") or wildcard suffixes
// ("*.pypi.org" matches "files.pypi.org" but not "pypi.org" itself).
// Exposed for tests; the proxy uses it internally on every request.
func MatchAllowlist(host string, allow []string) bool {
	if host == "" {
		return false
	}
	for _, pat := range allow {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if pat == host {
			return true
		}
		if strings.HasPrefix(pat, "*.") && strings.HasSuffix(host, pat[1:]) {
			return true
		}
	}
	return false
}

func (p *Proxy) audit(e AuditEvent) {
	p.auditMu.Lock()
	defer p.auditMu.Unlock()
	_ = p.auditEnc.Encode(e)
}

func hostnameOf(urlHost, reqHost string) string {
	h := urlHost
	if h == "" {
		h = reqHost
	}
	// Strip port if present.
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		// Rough IPv6-aware split; good enough for the prototype.
		host, _, err := net.SplitHostPort(h)
		if err == nil {
			return host
		}
	}
	return h
}
