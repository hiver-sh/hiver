// Package proxy implements the sandbox's MITM egress proxy.
//
// Scope: HTTP forward proxy + CONNECT tunneling for HTTPS, with
// host-pattern allowlist and JSON-line audit logging. No TLS body inspection
// (no per-sandbox CA), no body inspectors, no credential broker — all those
// belong to later tickets (T57, T60–T65).
package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"log"

	"github.com/klauspost/compress/zstd"
)

// Config drives a proxy instance.
type Config struct {
	// Listen address (e.g. "127.0.0.1:3128"). Required.
	Addr string
	// Rules lists egress rules evaluated top-to-bottom; the first match wins.
	// An empty list denies everything.
	Rules []EgressRule
	// Audit sink. Each event is encoded as one JSON line.
	// Required; tests pass *bytes.Buffer, prod passes os.Stderr.
	Audit io.Writer
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

	// UpstreamPoolShared selects the upstream connection-pool scope. false (vm)
	// keys the pool by source IP so co-tenant sandboxes are isolated (the safe
	// default). true (pod) drops srcIP from the key so one warm upstream
	// connection to a host is reused across ALL sandboxes in the pod — faster
	// (a fresh sandbox reuses a sibling's warm connection) but it gives up per-VM
	// connection isolation. See upstreamPool's doc for the trade-off.
	UpstreamPoolShared bool
}

// EgressRule is one egress rule for outbound traffic.
//
// Access ("allow" or "deny") is required and decides the outcome when this
// rule matches. Rules are evaluated top-to-bottom; the first match wins.
// Host is required (exact, or "*.suffix" wildcard). Ports, Methods, and
// Paths are optional filters — an empty list means "no enforcement on
// this dimension" (any port / any method / any path matches). Override,
// when set, lets the proxy inject URL query parameters and HTTP headers
// into the forwarded request; it is only applied for allow rules.
//
// For TLS in transparent mode, only Host and Ports are consulted (the
// proxy can read SNI from the ClientHello and the destination port
// from SO_ORIGINAL_DST, but not method/path under encryption);
// Methods/Paths/Override are ignored on those flows.
//
// Passthrough, when true, disables TLS interception for this rule even
// when the proxy is configured with a CA. The byte stream is forwarded
// end-to-end without termination, preserving the client's TLS fingerprint.
// Use this for hosts whose WAF or bot-detection rejects the proxy's TLS
// fingerprint (e.g. WebSocket endpoints protected by Cloudflare Bot
// Management). Method/Path/Override are not enforced for passthrough rules.
type EgressRule struct {
	Access      string          `json:"access"` // "allow" | "deny"
	Host        string          `json:"host"`
	Ports       []int           `json:"ports,omitempty"`
	Methods     []string        `json:"methods,omitempty"`
	Paths       []string        `json:"paths,omitempty"`
	Override    *EgressOverride `json:"override,omitempty"`
	Passthrough bool            `json:"passthrough,omitempty"`
}

// EgressOverride bundles per-rule URL query and header injections plus an
// optional upstream substitution. The maps are applied with Set semantics:
// an existing value (set by the agent) is replaced; an absent value is
// added. Host, when set ("hostname[:port]"), replaces the dial target for
// matching requests — matching and every agent-visible artifact (Host
// header, SNI, minted certs) still use the original hostname. PrefixPath,
// when set ("/mock"), is prepended to the outbound request path; matching
// and audit events keep the agent's original path.
//
// Body, when set, rewrites the request body. As a JSON string it always
// replaces the body verbatim. As a JSON object, BodyStrategy decides how it
// is applied: "merge" (the default) shallow-merges it into the agent's JSON
// body (top-level keys in the override win, the agent's other keys are
// preserved); "replace" discards the agent's body and sends the override
// object as-is. See [applyBodyOverride].
type EgressOverride struct {
	Host         string            `json:"host,omitempty"`
	PrefixPath   string            `json:"prefix_path,omitempty"`
	Query        map[string]string `json:"query,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         json.RawMessage   `json:"body,omitempty"`
	BodyStrategy string            `json:"body_strategy,omitempty"` // "merge" (default) | "replace"
}

// Body merge strategies for EgressOverride.BodyStrategy. An empty value is
// treated as BodyStrategyMerge to preserve the original object-merge behavior.
const (
	BodyStrategyMerge   = "merge"
	BodyStrategyReplace = "replace"
)

// AuditEvent is one record on the audit.network topic (§9.1).
//
// Each user-level request produces one phase:"request" event (access
// decision), one phase:"response" event (status + headers + time-to-first-
// byte, emitted as soon as the upstream responds), and zero or more
// phase:"response_chunk" events carrying body bytes as they flow. SSE and
// WebSocket streams emit many chunks; ordinary HTTP responses emit
// however many the upstream pushes per Read. All four share the same
// RequestID.
//
// For CONNECT and raw-forward TLS, only request/response events fire —
// the proxy doesn't see body bytes through the cipher.
type AuditEvent struct {
	At         time.Time         `json:"at"`
	Type       string            `json:"type"`  // always "network"
	Phase      string            `json:"phase"` // "request" | "response" | "response_chunk"
	RequestID  int               `json:"request_id"`
	SrcIP      string            `json:"src_ip,omitempty"` // workload source IP; used in pack mode to route to per-sandbox broker
	Method     string            `json:"method"`
	Host       string            `json:"host"`
	Path       string            `json:"path,omitempty"`
	Query      string            `json:"query,omitempty"`   // raw URL query (no leading "?")
	Headers    map[string]string `json:"headers,omitempty"` // HTTP headers; nil for TLS/CONNECT
	Body       string            `json:"body,omitempty"`    // request body (phase:request) or chunk bytes (phase:response_chunk); response events no longer carry a body
	Label      string            `json:"label,omitempty"`   // origin tag on response_chunk events; "up"/"down" for WebSocket, empty for HTTP
	Verdict    string            `json:"verdict"`           // "allow" | "deny" | "error"
	Status     int               `json:"status,omitempty"`
	DurationMs int               `json:"duration_ms,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Upstream   string            `json:"upstream,omitempty"` // dial target substituted by override.host; empty when not rewritten
}

// Proxy is the running proxy. Construct with [New], drive with [Run].
type Proxy struct {
	cfg          Config
	listener     net.Listener
	dialer       *net.Dialer
	transport    *http.Transport
	minter       *CertMinter   // nil unless TLS termination is enabled
	innerTLS     *tls.Config   // shared agent-facing server config (stable session-ticket keys)
	upstreamPool *upstreamPool // per-source keep-alive pool for the TLS-intercept upstream path

	// passthroughAll, when set (HIVER_PROXY_PASSTHROUGH_ALL), routes every TLS flow
	// to raw splice (no MITM termination) regardless of rule — a measurement knob to
	// isolate the proxy's MITM cost (intercept minus passthrough). Egress is still
	// SNI-host-checked and relayed; only decrypt/re-encrypt is skipped.
	passthroughAll bool

	// bySource holds egress rules keyed by the workload's source IP, so one
	// shared proxy can enforce a different allowlist per sandbox (design §8).
	// The "" key is the "all sources" bucket used in single-sandbox mode
	// (SetRules). A connection's source IP is matched first, then "", then
	// denied — so an unknown source on a per-source map is default-deny.
	// Swappable at runtime via SetRules/SetRulesBySource so sandboxd can
	// reconcile after a config change without restarting the proxy.
	bySource atomic.Pointer[map[string][]EgressRule]

	auditMu  sync.Mutex
	auditEnc *json.Encoder

	requestSeq atomic.Uint64 // source of AuditEvent.RequestID

	dnsDedupe dnsDedupe // collapses repeated DNS-sink lookups in the audit stream
}

// SetRules atomically replaces the egress rules for all sources (single-sandbox
// mode). Equivalent to SetRulesBySource with one "" bucket. Safe to call from
// any goroutine; in-flight matches see either the old or the new rules, never a
// torn read.
func (p *Proxy) SetRules(rules []EgressRule) {
	p.SetRulesBySource(map[string][]EgressRule{"": rules})
}

// SetRulesBySource atomically replaces the per-source egress rule map. Keys are
// workload source IPs; the "" key is the all-sources fallback. A copy is stored,
// so the caller may mutate its map afterward.
func (p *Proxy) SetRulesBySource(bySource map[string][]EgressRule) {
	cp := make(map[string][]EgressRule, len(bySource))
	for src, rules := range bySource {
		rs := make([]EgressRule, len(rules))
		copy(rs, rules)
		cp[src] = rs
	}
	p.bySource.Store(&cp)
}

// rulesForSource returns the egress rules that govern srcIP: the source's own
// bucket if present, else the all-sources "" bucket, else nil (deny). The
// returned slice is owned by the proxy — callers must not mutate it.
func (p *Proxy) rulesForSource(srcIP string) []EgressRule {
	m := p.bySource.Load()
	if m == nil {
		return nil
	}
	if rs, ok := (*m)[srcIP]; ok {
		return rs
	}
	if rs, ok := (*m)[""]; ok {
		return rs
	}
	return nil
}

// srcIPOf extracts the host portion of a "host:port" remote address, or returns
// the input unchanged if it carries no port.
func srcIPOf(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// New validates the config and returns a Proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.Addr == "" {
		return nil, errors.New("proxy: Addr required")
	}
	if cfg.Audit == nil {
		return nil, errors.New("proxy: Audit sink required")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if cfg.OutboundMark != 0 {
		ctrl := soMarkControl(cfg.OutboundMark)
		dialer.Control = ctrl
		// The proxy now dials upstreams by hostname (DNS is sinkholed for the
		// workload), so it does its own resolution. Those DNS sockets must
		// carry the same SO_MARK as upstream connections, otherwise the
		// iptables UDP/53 REDIRECT would bounce them into the sink and the
		// proxy would "resolve" every name to the placeholder. Force the Go
		// resolver and stamp its sockets too.
		dialer.Resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := &net.Dialer{Timeout: 5 * time.Second, Control: ctrl}
				return d.DialContext(ctx, network, address)
			},
		}
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
		dialer:       dialer,
		transport:    transport,
		auditEnc:     json.NewEncoder(cfg.Audit),
		upstreamPool: newUpstreamPool(cfg.UpstreamPoolShared),
	}
	if cfg.UpstreamPoolShared {
		log.Printf("proxy: upstream pool scope = pod — connections reused across sandboxes (per-VM isolation off)")
	}
	if cfg.CACert != nil && cfg.CAKey != nil {
		p.minter = NewCertMinter(cfg.CACert, cfg.CAKey)
		// ONE shared server config across every agent-facing handshake. The old
		// code built a fresh tls.Config per connection, so Go minted a new random
		// session-ticket key each time → a client could never resume, and every
		// inner handshake paid a full ECDHE. A shared config keeps Go's auto-rotated
		// ticket keys stable, so repeat connections from the same agent resume (1-RTT,
		// no cert, no full key exchange). GetCertificate keys off the handshake SNI so
		// one config serves all hosts. ALPN stays http/1.1 (the inspection loop reads
		// request lines, not HPACK). MinVersion stays 1.2 for client compatibility —
		// 1.3 is already preferred automatically whenever the client offers it.
		p.innerTLS = &tls.Config{
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return p.minter.Mint(hello.ServerName)
			},
			// Offer h2 first: Chrome then multiplexes a page over one connection
			// instead of opening ~6 parallel HTTP/1.1 ones (see serveInnerH2). Falls
			// back to http/1.1 for agents that don't speak h2. HIVER_PROXY_DISABLE_H2
			// drops back to http/1.1-only ALPN — a no-redeploy kill switch should the
			// h2 inner path misbehave.
			NextProtos: []string{"h2", "http/1.1"},
			MinVersion: tls.VersionTLS12,
		}
		if os.Getenv("HIVER_PROXY_DISABLE_H2") != "" {
			p.innerTLS.NextProtos = []string{"http/1.1"}
		}
	}
	if os.Getenv("HIVER_PROXY_PASSTHROUGH_ALL") != "" {
		p.passthroughAll = true
		log.Printf("proxy: HIVER_PROXY_PASSTHROUGH_ALL set — TLS interception disabled, raw splice for all flows")
	}
	p.SetRules(cfg.Rules)
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
	// Reap idle upstream connections and close the pool when the proxy stops.
	go p.upstreamPool.reap(ctx)
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
	ac := p.beginAudit(srcIPOf(r.RemoteAddr), r.Method, host, r.URL.Path, r.URL.RawQuery)
	log.Printf("handleHTTP: %s %s body_nil=%v", r.Method, r.URL, r.Body == nil)
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("handleHTTP: read body: %v", err)
		} else {
			r.Body = io.NopCloser(bytes.NewReader(b))
			ac.requestBody = decodeBytes(r.Header.Get("Content-Encoding"), b)
		}
	}
	ac.requestHeaders = headerMap(r.Header)
	rule := MatchEgress(p.rulesForSource(srcIPOf(r.RemoteAddr)), r.Method, host, port, r.URL.Path)
	if rule == nil || rule.Access == "deny" {
		ac.deny("no matching rule", http.StatusForbidden)
		http.Error(w, denyHTTPBody(host), http.StatusForbidden)
		return
	}
	if rule.Access == "allow" {
		applyOverride(r, rule.Override)
		ac.requestHeaders = headerMap(r.Header)
		// A body override rewrites r.Body in place; re-read it so the audit
		// stream reflects what the upstream actually receives.
		if rule.Override != nil && len(rule.Override.Body) > 0 && r.Body != nil {
			b, err := io.ReadAll(r.Body)
			if err == nil {
				r.Body = io.NopCloser(bytes.NewReader(b))
				ac.requestBody = decodeBytes(r.Header.Get("Content-Encoding"), b)
			}
		}
	}
	dialAddr := dialTarget(rule, net.JoinHostPort(host, strconv.Itoa(port)))
	overridden := rule.Override != nil && rule.Override.Host != ""
	if overridden {
		ac.upstream = dialAddr
	}
	ac.allow()

	// Rebuild the request for forwarding. http.DefaultTransport requires
	// RequestURI to be empty and URL to have a scheme + host.
	r.RequestURI = ""
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}
	if overridden {
		// Dial the override target; r.Host keeps the original hostname so
		// the upstream still sees the agent's Host header.
		r.URL.Host = dialAddr
	}

	if isWebSocketUpgrade(r) {
		p.handleWebSocket(w, r, dialAddr, ac)
		return
	}

	out, err := p.transport.RoundTrip(r)
	if err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer out.Body.Close()

	src := unwrapBody(out)
	ac.responseHeaders = headerMap(out.Header)

	for k, vs := range out.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(out.StatusCode)
	var flush func()
	if flusher, ok := w.(http.Flusher); ok {
		flush = flusher.Flush
	}
	// Emit the response event now — status, headers, and time-to-first-byte
	// are known. Body bytes flow as response_chunk events from chunkForward
	// so consumers don't have to wait for the body to finish.
	ac.response(out.StatusCode)
	p.chunkForward(src, w, flush, ac, isTextContentType(out.Header.Get("Content-Type")))
}

// decodeBytes decompresses b according to the Content-Encoding header value,
// returning the plain text. On any error it falls back to the raw bytes as a
// string so the audit log always gets something, even if it's binary.
func decodeBytes(enc string, b []byte) string {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "gzip":
		gr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			log.Printf("decodeBytes: gzip.NewReader: %v", err)
			return string(b)
		}
		out, err := io.ReadAll(gr)
		if err != nil {
			log.Printf("decodeBytes: gzip read: %v", err)
			return string(b)
		}
		return string(out)
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(b))
		if err != nil {
			log.Printf("decodeBytes: zstd.NewReader: %v", err)
			return string(b)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			log.Printf("decodeBytes: zstd read: %v", err)
			return string(b)
		}
		return string(out)
	default:
		return string(b)
	}
}

// unwrapBody returns an io.Reader for the body, decompressing gzip or zstd if
// the response carries a matching Content-Encoding. It deletes that header (and
// Content-Length, which becomes invalid) so the caller can forward the
// response without advertising compression to the client.
func unwrapBody(resp *http.Response) io.Reader {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch enc {
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return resp.Body
		}
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		return gr
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return resp.Body
		}
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		return zr
	default:
		return resp.Body
	}
}

// chunkForward copies src to dst one chunk at a time (whatever the server
// pushes in a single Read), flushing after each chunk. flush may be nil.
// Each chunk is emitted as a response_chunk audit event so consumers see body
// bytes as they arrive — the paired response event has already been emitted
// (with status + headers + time-to-first-byte) before this loop starts.
// logBody suppresses chunk events for binary content types.
func (p *Proxy) chunkForward(src io.Reader, dst io.Writer, flush func(), ac *auditCtx, logBody bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if logBody {
				ac.streamChunk(string(buf[:n]), "")
			}
			_, _ = dst.Write(buf[:n])
			if flush != nil {
				flush()
			}
		}
		if err != nil {
			break
		}
	}
}

// isTextContentType reports whether ct is a human-readable text type safe to
// include in audit logs. An empty Content-Type defaults to true. Binary types
// (image/*, audio/*, video/*, application/octet-stream, etc.) return false.
func isTextContentType(ct string) bool {
	if ct == "" {
		return true
	}
	mt, _, _ := mime.ParseMediaType(ct)
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/xml",
		"application/javascript", "application/x-javascript",
		"application/x-www-form-urlencoded",
		"application/ld+json", "application/graphql",
		"application/graphql+json", "application/atom+xml",
		"application/rss+xml":
		return true
	}
	return strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}

// isWebSocketUpgrade reports whether r carries a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// writeWebSocketUpgrade writes a WebSocket upgrade request to w using the
// headers from req.Header directly, without the modifications that
// http.Request.Write makes:
//
//   - req.Write adds "User-Agent: Go-http-client/1.1" when the original
//     request carries no User-Agent. That tag identifies the proxy to the
//     upstream's WAF and can trigger security rejections.
//   - req.Write sorts headers in map-iteration order, which may differ from
//     the client's original ordering.
//
// Header names are still Go-canonical (e.g. "Sec-Websocket-Key") because
// http.ReadRequest normalises them on ingress; that canonicalisation is
// unavoidable. HTTP/1.1 requires case-insensitive header processing, so
// compliant upstreams accept it.
func writeWebSocketUpgrade(w io.Writer, req *http.Request) error {
	bw := bufio.NewWriter(w)
	path := req.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	log.Printf("ws upgrade: sending %s %s HTTP/1.1 host=%s headers=%v", req.Method, path, req.Host, req.Header)
	if _, err := fmt.Fprintf(bw, "%s %s HTTP/1.1\r\nHost: %s\r\n", req.Method, path, req.Host); err != nil {
		return err
	}
	if err := req.Header.Write(bw); err != nil {
		return err
	}
	if _, err := io.WriteString(bw, "\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// handleWebSocket tunnels a plain ws:// WebSocket through the proxy.
// The egress rule has already been matched and ac.allow() already called by
// handleHTTP, which also resolved dialAddr (including any override.host
// substitution); this function owns the ac.response() call.
func (p *Proxy) handleWebSocket(w http.ResponseWriter, r *http.Request, dialAddr string, ac *auditCtx) {
	upstream, err := p.dialer.DialContext(r.Context(), "tcp", dialAddr)
	if err != nil {
		ac.responseError(err.Error(), http.StatusBadGateway)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Strip Sec-WebSocket-Extensions so the server can't negotiate
	// permessage-deflate (or anything else that touches the RSV bits).
	// Frames on the wire are then plain bytes and the audit log
	// records exactly the application payload.
	stripWebSocketExtensions(r.Header)
	if err := writeWebSocketUpgrade(upstream, r); err != nil {
		_ = upstream.Close()
		ac.responseError("ws write request: "+err.Error(), http.StatusBadGateway)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	upstreamBuf := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamBuf, r)
	if err != nil {
		_ = upstream.Close()
		ac.responseError("ws read response: "+err.Error(), http.StatusBadGateway)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = upstream.Close()
		ac.responseError("ws: upstream rejected upgrade", resp.StatusCode)
		http.Error(w, "upstream rejected WebSocket upgrade", resp.StatusCode)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		ac.responseError("ws: no hijacker", http.StatusInternalServerError)
		http.Error(w, "no hijacker", http.StatusInternalServerError)
		return
	}
	client, clientBrw, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		ac.responseError(err.Error(), http.StatusInternalServerError)
		return
	}

	if err := writeResponseHeaders(client, resp); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		ac.responseError("ws write response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Response event before the WS tunnel starts pumping — frame audit
	// events flow as response_chunks from wsForward, so consumers see the
	// 101 immediately rather than at tunnel close.
	ac.responseHeaders = headerMap(resp.Header)
	ac.response(http.StatusSwitchingProtocols)
	done := make(chan struct{}, 2)
	go func() {
		p.wsForward(io.MultiReader(clientBrw.Reader, client), upstream, wsDirUp, ac)
		done <- struct{}{}
	}()
	go func() {
		p.wsForward(io.MultiReader(upstreamBuf, upstream), client, wsDirDown, ac)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = upstream.Close()
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	// CONNECT always carries a port (e.g. "example.com:443"); 443 is
	// only used as a safety net for malformed inputs.
	host, port := splitHostPort("", r.Host, 443)
	ac := p.beginAudit(srcIPOf(r.RemoteAddr), "CONNECT", host, "", "")
	// CONNECT is host-only — body is opaque under TLS.
	rule := MatchEgress(p.rulesForSource(srcIPOf(r.RemoteAddr)), "CONNECT", host, port, "")
	if rule == nil || rule.Access == "deny" {
		ac.deny("no matching rule", http.StatusForbidden)
		http.Error(w, denyHTTPBody(host), http.StatusForbidden)
		return
	}
	// An override.host redirects the tunnel's TCP target only — the TLS
	// inside is end-to-end, so the override target must present a cert the
	// agent trusts for the original hostname.
	dialAddr := dialTarget(rule, r.Host)
	if rule.Override != nil && rule.Override.Host != "" {
		ac.upstream = dialAddr
	}

	upstream, err := p.dialer.DialContext(r.Context(), "tcp", dialAddr)
	if err != nil && err != io.EOF {
		// Tunnel dial failed before we ever allowed the tunnel: surface
		// as a deny so consumers don't see an orphan response event.
		ac.deny("upstream dial: "+err.Error(), http.StatusBadGateway)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	ac.requestHeaders = headerMap(r.Header)
	ac.allow()

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		ac.responseError("no hijacker", http.StatusInternalServerError)
		http.Error(w, "no hijacker", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		ac.responseError(err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		ac.responseError(err.Error(), http.StatusInternalServerError)
		return
	}

	// Bidi tunnel until either side closes.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = upstream.Close()
	ac.response(http.StatusOK)
}

// MatchEgress finds the first rule that matches a request, or nil if
// none does. The caller must check rule.Access ("allow"/"deny") to determine
// the outcome. method and path may be "" for non-HTTP traffic (TLS,
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
			// At connection level (TLS/CONNECT) we don't know the request
			// path yet. A deny rule with path restrictions cannot be
			// enforced here — skipping it avoids blocking the whole tunnel
			// for a path-specific deny. The HTTP-level check (after TLS
			// interception) will enforce it when the actual path is known.
			if r.Access == "deny" && len(r.Paths) > 0 {
				continue
			}
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

// applyOverride mutates r in-place to apply the rule's Override
// directives. Query keys are written via url.Values.Set (replacing any
// value the agent sent); header keys are written via Header.Set with
// the same semantics. r.URL.RawQuery is re-encoded only when the query
// override actually fires, so requests without an override pay no cost.
//
// Both maps are nil-safe: a nil Override, nil Query, or nil Headers is
// a no-op.
func applyOverride(r *http.Request, ov *EgressOverride) {
	if ov == nil {
		return
	}
	if len(ov.Query) > 0 && r.URL != nil {
		q := r.URL.Query()
		for k, v := range ov.Query {
			q.Set(k, v)
		}
		r.URL.RawQuery = q.Encode()
	}
	for k, v := range ov.Headers {
		r.Header.Set(k, v)
	}
	if ov.PrefixPath != "" && r.URL != nil {
		prefix := strings.TrimSuffix(ov.PrefixPath, "/")
		r.URL.Path = prefix + r.URL.Path
		// Keep RawPath consistent so EscapedPath() doesn't silently fall
		// back to re-encoding Path when the original had escaped octets.
		if r.URL.RawPath != "" {
			r.URL.RawPath = prefix + r.URL.RawPath
		}
	}
	applyBodyOverride(r, ov.Body, ov.BodyStrategy)
}

// applyBodyOverride rewrites r's body per the override directive, a no-op
// when body is empty. A JSON string value replaces the body verbatim with
// the decoded string. A JSON object value is applied per strategy:
//   - "merge" (the default, also used for an empty strategy) shallow-merges
//     the override into the agent's JSON request body: the agent's body is
//     parsed as a JSON object, the override's top-level keys overwrite the
//     agent's, and any keys the override omits are preserved (agent
//     {"a":1,"b":2} + override {"b":3} → {"a":1,"b":3}). Merging is JSON-only:
//     when the agent's body is absent or not a JSON object, the override
//     object is sent as-is.
//   - "replace" discards the agent's body entirely and sends the override
//     object as-is.
//
// Anything other than a JSON string or object is ignored. On a rewrite the
// body reader, ContentLength, and Content-Length header are reset and
// Content-Encoding is dropped (the new body is plaintext).
func applyBodyOverride(r *http.Request, body json.RawMessage, strategy string) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return
	}
	var newBody []byte
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return
		}
		newBody = []byte(s)
	case '{':
		var override map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &override); err != nil {
			return
		}
		merged := map[string]json.RawMessage{}
		if r.Body != nil {
			orig, err := io.ReadAll(r.Body)
			r.Body.Close()
			// On "replace" the agent's body is dropped, but it must still be
			// drained and closed so the reader isn't reused.
			if err == nil && strategy != BodyStrategyReplace {
				// Best-effort: a non-object or invalid original body leaves
				// merged empty, so the override object is sent on its own.
				_ = json.Unmarshal(orig, &merged)
			}
		}
		for k, v := range override {
			merged[k] = v
		}
		b, err := json.Marshal(merged)
		if err != nil {
			return
		}
		newBody = b
	default:
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	r.Header.Del("Content-Encoding")
}

// upstreamAddr is the address the proxy dials for a transparent connection.
// With DNS sinkholed for the workload, origDst is a placeholder, so we dial the
// matched hostname (re-resolved on the proxy's own SO_MARK'd resolver) and keep
// the real destination port from origDst. When no name was observed — host fell
// back to origDst, e.g. a raw IP-literal connection — origDst is the real
// destination and we use it unchanged. host may carry a port (HTTP Host header);
// the port from origDst always wins since that's what the kernel actually dialed.
func upstreamAddr(host, origDst string) string {
	hostOnly, _ := splitHostPort("", host, 0)
	_, port := splitHostPort("", origDst, 0)
	if hostOnly == "" || port == 0 {
		return origDst
	}
	return net.JoinHostPort(hostOnly, strconv.Itoa(port))
}

// dialTarget returns the address the proxy dials for a request matched by
// rule. A rule with override.host substitutes the upstream: its port wins
// when present, otherwise the port of def (the address that would have been
// dialed) is kept. Without an override, def is returned unchanged. When
// neither the override nor def carries a port (explicit-proxy URLs with a
// scheme-default port), the bare hostname is returned and the caller's
// scheme default applies.
func dialTarget(rule *EgressRule, def string) string {
	if rule == nil || rule.Override == nil || rule.Override.Host == "" {
		return def
	}
	host, port := splitHostPort("", rule.Override.Host, 0)
	if port == 0 {
		_, port = splitHostPort("", def, 0)
	}
	if host == "" {
		return def
	}
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func matchHost(pat, host string) bool {
	pat = strings.TrimSpace(pat)
	if pat == "" || host == "" {
		return false
	}
	return globMatch(pat, host)
}

// globMatch implements glob-style matching where '*' matches any sequence of
// characters (including dots). This allows patterns like "*.host", "*.host.*",
// or bare "*" to express host allowlists concisely.
func globMatch(pat, s string) bool {
	i := strings.IndexByte(pat, '*')
	if i < 0 {
		return pat == s
	}
	if !strings.HasPrefix(s, pat[:i]) {
		return false
	}
	s = s[i:]       // consume the fixed prefix; s now starts where '*' was matched
	pat = pat[i+1:] // advance past the '*'
	for j := 0; j <= len(s); j++ {
		if globMatch(pat, s[j:]) {
			return true
		}
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

// matchPath reports whether path matches any of the allowed glob patterns.
// An empty list means "any path matches".
func matchPath(allowed []string, path string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if matchGlob(a, path) {
			return true
		}
	}
	return false
}

// matchGlob matches a git-style path glob against path. The pattern and path
// are compared segment by segment on "/" boundaries:
//   - "*" matches zero or more characters within a single segment; it never
//     crosses "/". "/a*" matches "/a" and "/abc" but not "/a/b".
//   - "**" as a whole segment matches zero or more segments. "/a/**" matches
//     "/a", "/a/b", and "/a/b/c"; "/**/c" matches "/c" and "/a/b/c".
//
// A literal segment must match exactly. Patterns and paths are expected to
// share the same leading "/" (both split to a leading "" segment).
func matchGlob(pattern, path string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

// matchSegments matches pattern segments pat against path segments seg, with
// "**" matching zero or more whole segments. It recurses on "**" to try every
// split point; other segments are matched one-to-one via matchSegment.
func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			// "**" matches zero or more segments: try consuming 0..len(seg)
			// of them and matching the remaining pattern against the rest.
			for i := 0; i <= len(seg); i++ {
				if matchSegments(rest, seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		if !matchSegment(pat[0], seg[0]) {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}

// matchSegment matches a single pattern segment against a single path segment,
// where "*" matches zero or more characters (any "**" within a segment, not
// bordered by "/", collapses to the same wildcard). All other characters are
// matched literally. Implemented as the classic greedy two-pointer wildcard
// match with backtracking on the most recent "*".
func matchSegment(pat, seg string) bool {
	if !strings.Contains(pat, "*") {
		return pat == seg
	}
	var s, p, star, matched int
	star = -1
	for s < len(seg) {
		switch {
		case p < len(pat) && pat[p] == '*':
			star, matched = p, s
			p++
		case p < len(pat) && pat[p] == seg[s]:
			p++
			s++
		case star != -1:
			p = star + 1
			matched++
			s = matched
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}

func (p *Proxy) audit(e AuditEvent) {
	p.auditMu.Lock()
	defer p.auditMu.Unlock()
	_ = p.auditEnc.Encode(e)
}

// EmitControl writes one JSON-line control record on the audit sink, serialized
// through the same encoder (and lock) as audit events so it never interleaves
// mid-record. sbxproxy uses it to signal out-of-band state — the applied
// egress-rules generation — back to sandboxd over the events fd that already
// carries audit events, avoiding a second channel.
func (p *Proxy) EmitControl(v any) {
	p.auditMu.Lock()
	defer p.auditMu.Unlock()
	_ = p.auditEnc.Encode(v)
}

// auditCtx tracks state across the request/response audit pair: the
// shared RequestID and the start time used to compute DurationMs on
// the response side. Callers invoke .allow()/.deny() at decision time
// and .response()/.responseError() once the upstream call finishes.
type auditCtx struct {
	p               *Proxy
	requestID       int
	srcIP           string
	start           time.Time
	method          string
	host            string
	path            string
	query           string
	requestHeaders  map[string]string // outbound headers after strip+override; nil for CONNECT/TLS
	requestBody     string            // request body
	responseHeaders map[string]string // upstream response headers; nil for CONNECT/TLS
	upstream        string            // dial target substituted by override.host; empty when not rewritten
}

// beginAudit allocates an auditCtx for one request/response pair.
// `query` is the raw URL query without the leading "?" — empty for
// CONNECT, raw-forward TLS, and any request without a query string.
// It rides alongside `path` to the audit event so the SSE
// `egress.request` consumer gets `path` and `query` as separate
// fields, while allowlist matching keeps using just `path`.
func (p *Proxy) beginAudit(srcIP, method, host, path, query string) *auditCtx {
	n := p.requestSeq.Add(1)
	return &auditCtx{
		p:         p,
		requestID: int(n),
		srcIP:     srcIP,
		start:     time.Now(),
		method:    method,
		host:      host,
		path:      path,
		query:     query,
	}
}

// deny emits a request event with verdict="deny" followed immediately by a
// paired response event. The proxy never reaches an upstream for denied
// requests, so the response is synthetic — Status carries the HTTP status
// the proxy returns to the client (typically 403), and DurationMs is the
// wall-clock time spent before the decision was made.
func (a *auditCtx) deny(reason string, status int) {
	now := time.Now()
	a.p.audit(AuditEvent{
		At: a.start, Type: "network", Phase: "request",
		RequestID: a.requestID, SrcIP: a.srcIP,
		Method: a.method, Host: a.host, Path: a.path, Query: a.query,
		Headers: a.requestHeaders,
		Body:    a.requestBody,
		Verdict: "deny", Status: status, Reason: reason,
	})
	a.p.audit(AuditEvent{
		At: now, Type: "network", Phase: "response",
		RequestID: a.requestID, SrcIP: a.srcIP,
		Method: a.method, Host: a.host, Path: a.path, Query: a.query,
		Verdict: "deny", Status: status, Reason: reason,
		DurationMs: int(now.Sub(a.start) / time.Millisecond),
	})
}

// allow emits the phase:"request" audit event immediately. It must be
// called before any response_chunk events so the sidecar correlator has
// the SSE request id in place when chunks arrive.
func (a *auditCtx) allow() {
	a.p.audit(AuditEvent{
		At: a.start, Type: "network", Phase: "request",
		RequestID: a.requestID, SrcIP: a.srcIP,
		Method: a.method, Host: a.host, Path: a.path, Query: a.query,
		Headers:  a.requestHeaders,
		Body:     a.requestBody,
		Verdict:  "allow",
		Upstream: a.upstream,
	})
}

// response emits the phase:"response" event with status, headers, and
// time-to-first-byte. Body bytes are NOT included — they flow as
// phase:"response_chunk" events from chunkForward / wsForward as they arrive,
// so consumers see the response immediately instead of waiting for the body
// to complete (matters most for long-lived SSE / WebSocket).
func (a *auditCtx) response(status int) {
	a.p.audit(AuditEvent{
		At: time.Now(), Type: "network", Phase: "response",
		RequestID: a.requestID, SrcIP: a.srcIP,
		Method: a.method, Host: a.host, Path: a.path, Query: a.query,
		Headers: a.responseHeaders,
		Verdict: "allow", Status: status,
		DurationMs: int(time.Since(a.start) / time.Millisecond),
	})
}

// streamChunk records a single body chunk on the in-flight request.
// label is optional ("up"/"down" for WebSocket directions; empty
// for HTTP/SSE where direction is implicit).
func (a *auditCtx) streamChunk(body, label string) {
	a.p.audit(AuditEvent{
		At: time.Now(), Type: "network", Phase: "response_chunk",
		RequestID: a.requestID, SrcIP: a.srcIP,
		Method: a.method, Host: a.host, Path: a.path, Query: a.query,
		Body: body, Label: label, Verdict: "allow",
	})
}

func (a *auditCtx) responseError(reason string, status int) {
	a.p.audit(AuditEvent{
		At: time.Now(), Type: "network", Phase: "response",
		RequestID: a.requestID, SrcIP: a.srcIP,
		Method: a.method, Host: a.host, Path: a.path, Query: a.query,
		Verdict: "error", Status: status, Reason: reason,
		DurationMs: int(time.Since(a.start) / time.Millisecond),
	})
}

// denyHTTPBody is the plain-text 403 body sent on an egress deny.
// Spelled out (vs. the original "egress denied: <host>") so an agent
// reading the response sees actionable text: which host, why, what
// to do about it.
func denyHTTPBody(host string) string {
	return fmt.Sprintf(
		"egress denied: no matching allow rule for %s. Add one under `egress` in the sandbox config.",
		host,
	)
}

// writeDenyHTTP writes a 403 with that body straight to a hijacked /
// raw net.Conn (the transparent HTTP path doesn't have an
// http.ResponseWriter to hand to http.Error).
func writeDenyHTTP(c net.Conn, host string) {
	body := denyHTTPBody(host)
	_, _ = fmt.Fprintf(c,
		"HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body,
	)
}

// writeResponseHeaders writes the HTTP status line and headers of resp to w,
// followed by the blank line that separates headers from body. Used when the
// caller needs to stream the body separately (e.g. SSE or WebSocket).
func writeResponseHeaders(w io.Writer, resp *http.Response) error {
	statusText := http.StatusText(resp.StatusCode)
	if statusText == "" {
		statusText = "Unknown"
	}
	if _, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText); err != nil {
		return err
	}
	// Forward upstream headers verbatim except Transfer-Encoding: the body
	// we're about to stream is already decoded by Go's http.ReadResponse.
	hdrs := resp.Header.Clone()
	hdrs.Del("Transfer-Encoding")
	if err := hdrs.Write(w); err != nil {
		return err
	}
	_, err := fmt.Fprint(w, "\r\n")
	return err
}

// headerMap converts an http.Header into a flat map[string]string, joining
// multi-value headers with ", " per RFC 7230 §3.2.2. Returns nil for an
// empty header set so the audit JSON omits the field entirely.
func headerMap(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	m := make(map[string]string, len(h))
	for k, vs := range h {
		m[k] = strings.Join(vs, ", ")
	}
	return m
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
