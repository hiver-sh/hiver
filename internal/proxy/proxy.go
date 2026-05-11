// Package proxy implements the sandbox's MITM egress proxy.
//
// Scope: HTTP forward proxy + CONNECT tunneling for HTTPS, with
// host-pattern allowlist and JSON-line audit logging. No TLS body inspection
// (no per-sandbox CA), no body inspectors, no credential broker — all those
// belong to later tickets (T57, T60–T65).
package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
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

// Config drives a proxy instance.
type Config struct {
	// Listen address (e.g. "127.0.0.1:3128"). Required.
	Addr string
	// Allow lists rules evaluated top-to-bottom; the first match wins.
	// An empty list denies everything.
	Allow []EgressRule
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
	// CACert / CAKey, when both set, enable TLS termination for rules
	// that carry path/method/header criteria. The proxy mints per-host
	// leaf certs signed by this CA; the orchestrator must install the
	// CA into the agent's trust store. If unset, transparent TLS is
	// always raw-forwarded after SNI host match.
	CACert *x509.Certificate
	CAKey  *ecdsa.PrivateKey
}

// EgressRule is one allow entry for outbound traffic.
//
// Host is required (exact, or "*.suffix" wildcard). Methods and Paths
// are optional HTTP filters — an empty list means "any". Headers, when
// set, are merged into the forwarded HTTP request via Header.Set
// (existing values are replaced).
//
// For TLS in transparent mode, only Host is consulted (the proxy can
// read SNI from the ClientHello but not method/path under encryption);
// Methods/Paths/Headers are ignored on those flows.
type EgressRule struct {
	Host    string            `json:"host"`
	Methods []string          `json:"methods,omitempty"`
	Paths   []string          `json:"paths,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
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
	Verdict string    `json:"verdict"` // "allow" | "deny" | "error"
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
	minter       *CertMinter // nil unless TLS termination is enabled

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
	p := &Proxy{
		cfg:          cfg,
		stripHeaders: strip,
		dialer:       dialer,
		transport:    transport,
		auditEnc:     json.NewEncoder(cfg.Audit),
	}
	if cfg.CACert != nil && cfg.CAKey != nil {
		p.minter = NewCertMinter(cfg.CACert, cfg.CAKey)
	}
	return p, nil
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
	rule := MatchEgress(p.cfg.Allow, r.Method, host, r.URL.Path)
	if rule == nil {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: r.Method,
			Host: host, Path: r.URL.Path, Verdict: "deny",
			Status: http.StatusForbidden, Reason: "no matching rule",
		})
		http.Error(w, "egress denied: "+host, http.StatusForbidden)
		return
	}

	for _, h := range p.stripHeaders {
		r.Header.Del(h)
	}
	for k, v := range rule.Headers {
		r.Header.Set(k, v)
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
	// CONNECT is host-only — body is opaque under TLS.
	if MatchEgress(p.cfg.Allow, "CONNECT", host, "") == nil {
		p.audit(AuditEvent{
			At: time.Now(), Type: "network", Method: "CONNECT",
			Host: host, Verdict: "deny", Status: http.StatusForbidden,
			Reason: "no matching rule",
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

// MatchEgress finds the first rule that matches a request, or nil if
// none does. method and path may be "" for non-HTTP traffic (TLS,
// CONNECT) — in that case only Host is matched.
func MatchEgress(rules []EgressRule, method, host, path string) *EgressRule {
	if host == "" {
		return nil
	}
	for i := range rules {
		r := &rules[i]
		if !matchHost(r.Host, host) {
			continue
		}
		if path == "" {
			return r
		}
		if !matchMethod(r.Methods, method) {
			continue
		}
		if !matchPath(r.Paths, path) {
			continue
		}
		return r
	}
	return nil
}

func matchHost(pat, host string) bool {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return false
	}
	if pat == host {
		return true
	}
	if strings.HasPrefix(pat, "*.") && strings.HasSuffix(host, pat[1:]) {
		return true
	}
	return false
}

func matchMethod(allowed []string, method string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(a, method) {
			return true
		}
	}
	return false
}

// matchPath supports exact match and a trailing "/*" wildcard. "/api/*"
// matches "/api" and anything under it; "/api" matches only itself.
func matchPath(allowed []string, path string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == path {
			return true
		}
		if strings.HasSuffix(a, "/*") {
			prefix := strings.TrimSuffix(a, "/*")
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return true
			}
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
		// Rough IPv6-aware split;
		host, _, err := net.SplitHostPort(h)
		if err == nil {
			return host
		}
	}
	return h
}
