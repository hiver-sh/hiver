//go:build linux

package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"

	"github.com/hiver-sh/hiver/internal/wsaudit"
	"golang.org/x/net/http2"
)

// serveInnerH2 serves an agent-facing connection that negotiated HTTP/2. Chrome
// multiplexes a page's many requests over this ONE connection instead of opening
// up to 6 parallel HTTP/1.1 connections — collapsing 6× inner TLS handshakes to 1,
// the dominant per-page egress cost. http2.Server.ServeConn handles framing/HPACK/
// flow-control and dispatches each stream (concurrently) to the handler, which
// rule-checks + audits + forwards exactly like the 1.1 path. It blocks until the
// connection closes; the caller (interceptTLS) closes clientTLS on return.
//
// Upstream stays HTTP/1.1 + uTLS (JA3 preserved) over the per-source pool — so the
// multiplexed inner requests reuse a small set of warm upstream conns, keeping the
// egress footprint low without an h2 upstream fingerprint to maintain.
func (p *Proxy) serveInnerH2(clientTLS net.Conn, sniHost, origDst, srcIP string, _ *EgressRule) {
	_, port := splitHostPort("", origDst, 0)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.handleInnerH2(w, r, sniHost, origDst, port, srcIP)
	})
	(&http2.Server{}).ServeConn(clientTLS, &http2.ServeConnOpts{Handler: h})
}

// handleInnerH2 serves one HTTP/2 stream: per-request rule enforcement + audit,
// then forward over the per-source upstream pool. Rule matching uses the stream's
// own authority (r.Host) so a coalesced request to a different host is still
// checked against its own rule; the upstream TLS identity / pool key uses the
// connection's SNI host (the cert we present).
func (p *Proxy) handleInnerH2(w http.ResponseWriter, r *http.Request, sniHost, origDst string, port int, srcIP string) {
	host := r.Host
	if host == "" {
		host = sniHost
	}
	ac := p.beginAudit(srcIP, r.Method, host, r.URL.Path, r.URL.RawQuery)

	// We don't enable h2 extended CONNECT, so Chrome won't send WebSocket/CONNECT
	// over h2 — fail closed if one somehow arrives rather than mishandle it.
	if r.Method == http.MethodConnect || wsaudit.IsUpgrade(r) {
		ac.deny("h2 connect/upgrade not supported", http.StatusNotImplemented)
		http.Error(w, "", http.StatusNotImplemented)
		return
	}

	rule := p.applyRequestRule(r, host, port, srcIP, ac, func() {
		http.Error(w, "", http.StatusForbidden)
	})
	if rule == nil {
		return // denied; status already written
	}

	dialAddr := dialTarget(rule, upstreamAddr(host, origDst))
	// Pool key leads with srcIP → a connection is only ever reused by the sandbox
	// that opened it (co-tenant isolation). TLS identity is the SNI host.
	upstreamConn, ubr := p.upstreamPool.get(srcIP, dialAddr, sniHost)
	if upstreamConn == nil {
		c, err := p.dialUpstreamTLS(dialAddr, sniHost, nil, false)
		if err != nil {
			ac.responseError("upstream: "+err.Error(), http.StatusBadGateway)
			http.Error(w, "", http.StatusBadGateway)
			return
		}
		upstreamConn, ubr = c, bufio.NewReader(c)
	}

	if p.forwardH2Upstream(w, r, upstreamConn, ubr, ac) {
		p.upstreamPool.put(srcIP, dialAddr, sniHost, upstreamConn, ubr)
	} else {
		_ = upstreamConn.Close()
	}
}

// forwardH2Upstream sends one h2 stream's request to an HTTP/1.1 upstream and
// streams the response back through the h2 ResponseWriter, auditing as it goes.
// Returns true when the upstream conn ended keep-alive-clean and may be pooled.
// Mirrors forwardHTTP's reuse logic; the only difference is the response sink is
// an http.ResponseWriter (h2 stream) rather than a raw connection.
func (p *Proxy) forwardH2Upstream(w http.ResponseWriter, r *http.Request, upstream net.Conn, ubr *bufio.Reader, ac *auditCtx) bool {
	keepAlive := !r.Close // reuse the upstream conn unless the request forbids it

	// writeUpstreamRequest writes req in HTTP/1.1 wire format (req.Write), converting
	// the h2 request to 1.1 for the upstream. applyRequestRule already cleared
	// RequestURI, which req.Write requires.
	if err := writeUpstreamRequest(upstream, r, nil, false, keepAlive); err != nil {
		ac.responseError("write request: "+err.Error(), http.StatusBadGateway)
		http.Error(w, "", http.StatusBadGateway)
		return false
	}
	resp, err := http.ReadResponse(ubr, r)
	if err != nil {
		ac.responseError("read response: "+err.Error(), http.StatusBadGateway)
		http.Error(w, "", http.StatusBadGateway)
		return false
	}
	defer resp.Body.Close()

	// unwrapBody transparently decompresses (and strips Content-Encoding/Length) so
	// audit sees plaintext; must run before copying headers to the writer.
	src := unwrapBody(resp)
	copyResponseHeaders(w.Header(), resp.Header)
	ac.responseHeaders = headerMap(resp.Header)
	w.WriteHeader(resp.StatusCode)
	ac.response(resp.StatusCode)

	var flush func()
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}
	p.chunkForward(src, w, flush, ac, isTextContentType(resp.Header.Get("Content-Type")))

	if !keepAlive {
		return false
	}
	// Reuse only if the upstream body fully drained (next pooled response starts
	// clean) and the upstream agreed to keep-alive.
	n, drainErr := io.CopyN(io.Discard, resp.Body, maxReuseDrainBytes)
	fullyDrained := drainErr == io.EOF || (drainErr == nil && n < maxReuseDrainBytes)
	return fullyDrained && !resp.Close && resp.ProtoAtLeast(1, 1) && ubr.Buffered() == 0
}

// hopByHopHeaders are connection-scoped HTTP/1.1 headers that are illegal in
// HTTP/2 responses; they must be dropped when relaying a 1.1 upstream response
// onto an h2 stream (the h2 writer manages framing itself).
var hopByHopHeaders = map[string]bool{
	"Connection":        true,
	"Proxy-Connection":  true,
	"Keep-Alive":        true,
	"Transfer-Encoding": true,
	"Te":                true,
	"Trailer":           true,
	"Upgrade":           true,
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
