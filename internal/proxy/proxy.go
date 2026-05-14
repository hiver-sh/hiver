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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// Host is required (exact, or "*.suffix" wildcard). Ports, Methods, and
// Paths are optional filters — an empty list means "no enforcement on
// this dimension" (any port / any method / any path matches). Headers,
// when set, are merged into the forwarded HTTP request via Header.Set
// (existing values are replaced).
//
// For TLS in transparent mode, only Host and Ports are consulted (the
// proxy can read SNI from the ClientHello and the destination port
// from SO_ORIGINAL_DST, but not method/path under encryption);
// Methods/Paths/Headers are ignored on those flows.
type EgressRule struct {
	Host    string            `json:"host"`
	Ports   []int             `json:"ports,omitempty"`
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
//
// Each user-level request produces a pair of events sharing the same
// RequestID: a `phase:"request"` carrying the access decision, then
// (for allowed HTTP flows that reach upstream) a `phase:"response"`
// carrying the upstream Status and DurationMs. CONNECT and raw-forward
// TLS emit request only — there's no HTTP-level response to report.
type AuditEvent struct {
	At         time.Time `json:"at"`
	Type       string    `json:"type"` // always "network"
	Phase      string    `json:"phase"`           // "request" | "response"
	RequestID  string    `json:"request_id"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	Path       string    `json:"path,omitempty"`
	Verdict    string    `json:"verdict"` // "allow" | "deny" | "error"
	Status     int       `json:"status,omitempty"`
	DurationMs int       `json:"duration_ms,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

// Proxy is the running proxy. Construct with [New], drive with [Run].
type Proxy struct {
	cfg          Config
	listener     net.Listener
	stripHeaders []string
	dialer       *net.Dialer
	transport    *http.Transport
	minter       *CertMinter // nil unless TLS termination is enabled

	// allow holds the current allowlist, swappable at runtime via
	// SetAllow so sandboxd can reconcile after a /v1/config PUT
	// without restarting the proxy.
	allow atomic.Pointer[[]EgressRule]

	auditMu  sync.Mutex
	auditEnc *json.Encoder

	requestSeq atomic.Uint64 // source of AuditEvent.RequestID
}

// SetAllow atomically replaces the egress allowlist. Safe to call from
// any goroutine; in-flight matches see either the old or the new list,
// never a torn read.
func (p *Proxy) SetAllow(rules []EgressRule) {
	cp := make([]EgressRule, len(rules))
	copy(cp, rules)
	p.allow.Store(&cp)
}

// currentAllow returns the live allowlist. The returned slice is
// owned by the proxy — callers must not mutate it.
func (p *Proxy) currentAllow() []EgressRule {
	if r := p.allow.Load(); r != nil {
		return *r
	}
	return nil
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
	p.SetAllow(cfg.Allow)
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
	// Default port matches the URL scheme: explicit-proxy HTTP carries
	// proxy-form URLs (http://… or https://…) and the scheme decides
	// the default when the agent omits the port. Without scheme info
	// fall back to 80.
	defPort := 80
	if r.URL.Scheme == "https" {
		defPort = 443
	}
	host, port := splitHostPort(r.URL.Host, r.Host, defPort)
	ac := p.beginAudit(r.Method, host, r.URL.Path)
	rule := MatchEgress(p.currentAllow(), r.Method, host, port, r.URL.Path)
	if rule == nil {
		ac.deny("no matching rule", http.StatusForbidden)
		http.Error(w, "egress denied: "+host, http.StatusForbidden)
		return
	}
	ac.allow()

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
		ac.responseError(err.Error(), http.StatusBadGateway)
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

	ac.response(out.StatusCode)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	// CONNECT always carries a port (e.g. "example.com:443"); 443 is
	// only used as a safety net for malformed inputs.
	host, port := splitHostPort("", r.Host, 443)
	ac := p.beginAudit("CONNECT", host, "")
	// CONNECT is host-only — body is opaque under TLS.
	if MatchEgress(p.currentAllow(), "CONNECT", host, port, "") == nil {
		ac.deny("no matching rule", http.StatusForbidden)
		http.Error(w, "egress denied: "+host, http.StatusForbidden)
		return
	}

	upstream, err := p.dialer.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		// Tunnel dial failed before we ever allowed the tunnel: surface
		// as a deny so consumers don't see an orphan response event.
		ac.deny("upstream dial: "+err.Error(), http.StatusBadGateway)
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

	ac.allow()

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
// CONNECT) — in that case Method/Path are not enforced; Host and Port
// still are. port=0 means "destination port unknown"; rules that pin
// Ports will never match in that case.
func MatchEgress(rules []EgressRule, method, host string, port int, path string) *EgressRule {
	if host == "" {
		return nil
	}
	for i := range rules {
		r := &rules[i]
		if !matchHost(r.Host, host) {
			continue
		}
		if !matchPort(r.Ports, port) {
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

// matchPort reports whether port is permitted by the rule. An empty
// allowed list means "no port enforcement" — any port matches.
func matchPort(allowed []int, port int) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == port {
			return true
		}
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

// auditCtx tracks state across the request/response audit pair: the
// shared RequestID and the start time used to compute DurationMs on
// the response side. Callers invoke .allow()/.deny() at decision time
// and .response()/.responseError() once the upstream call finishes.
type auditCtx struct {
	p              *Proxy
	requestID      string
	start          time.Time
	method         string
	host           string
	path           string
}

func (p *Proxy) beginAudit(method, host, path string) *auditCtx {
	n := p.requestSeq.Add(1)
	return &auditCtx{
		p:         p,
		requestID: strconv.FormatUint(n, 10),
		start:     time.Now(),
		method:    method,
		host:      host,
		path:      path,
	}
}

// deny emits a request event with verdict="deny". `status` is the HTTP
// status the proxy will return to the client (typically 403), and is
// surfaced for human debugging — it isn't part of the SSE
// egress.request shape.
func (a *auditCtx) deny(reason string, status int) {
	a.p.audit(AuditEvent{
		At: a.start, Type: "network", Phase: "request",
		RequestID: a.requestID,
		Method:    a.method, Host: a.host, Path: a.path,
		Verdict: "deny", Status: status, Reason: reason,
	})
}

// allow emits a request event with verdict="allow". Callers must
// follow up with response()/responseError() if the request reaches an
// upstream that returns an HTTP status (i.e. HTTP and intercepted-TLS
// paths). CONNECT and raw-forward TLS skip response — the schema's
// egress.response is HTTP-status-shaped.
func (a *auditCtx) allow() {
	a.p.audit(AuditEvent{
		At: a.start, Type: "network", Phase: "request",
		RequestID: a.requestID,
		Method:    a.method, Host: a.host, Path: a.path,
		Verdict: "allow",
	})
}

func (a *auditCtx) response(status int) {
	a.p.audit(AuditEvent{
		At: time.Now(), Type: "network", Phase: "response",
		RequestID: a.requestID,
		Method:    a.method, Host: a.host, Path: a.path,
		Verdict: "allow", Status: status,
		DurationMs: int(time.Since(a.start) / time.Millisecond),
	})
}

func (a *auditCtx) responseError(reason string, status int) {
	a.p.audit(AuditEvent{
		At: time.Now(), Type: "network", Phase: "response",
		RequestID: a.requestID,
		Method:    a.method, Host: a.host, Path: a.path,
		Verdict: "error", Status: status, Reason: reason,
		DurationMs: int(time.Since(a.start) / time.Millisecond),
	})
}

// splitHostPort returns the hostname and port from one of urlHost
// (proxy-form, may be empty) or reqHost (Host header). If neither
// carries a port, defaultPort is returned. defaultPort=0 means
// "unknown" — port-based matching against that value will only succeed
// for rules without a Ports filter.
func splitHostPort(urlHost, reqHost string, defaultPort int) (string, int) {
	h := urlHost
	if h == "" {
		h = reqHost
	}
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		host, portStr, err := net.SplitHostPort(h)
		if err == nil {
			if port, err := strconv.Atoi(portStr); err == nil {
				return host, port
			}
			return host, defaultPort
		}
	}
	return h, defaultPort
}
