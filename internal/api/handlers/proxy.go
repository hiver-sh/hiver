package handlers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/wsaudit"
)

// proxyDialHold bounds how long the ingress dialer waits for the workload to
// start listening on an exposed port before giving up.
const proxyDialHold = 60 * time.Second

// maxBodyCapture is the per-request and per-chunk body capture limit (512 KB).
const maxBodyCapture = 512 * 1024

// streamingWriter wraps gin.ResponseWriter so a proxied inbound response is
// audited the way the egress proxy audits an outbound one: an `ingress.response`
// event is emitted as soon as the status and headers are known (carrying
// time-to-first-byte, not the body), then each body write is relayed live as an
// `ingress.chunk` event. This lets consumers watch long-lived SSE streams as
// they arrive instead of seeing nothing until the stream closes. It embeds the
// gin.ResponseWriter so the reverse proxy's Flush / Hijack / CloseNotify still
// reach the real writer.
type streamingWriter struct {
	gin.ResponseWriter
	h         *Sandbox
	reqID     int64
	start     time.Time
	published bool // ingress.response already emitted
	logBody   bool // response body is capturable text (set with the response)
}

func (w *streamingWriter) WriteHeader(code int) {
	w.ensureResponse(code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *streamingWriter) Write(b []byte) (int, error) {
	// A write before an explicit WriteHeader is an implicit 200 (net/http
	// semantics); publish the response first so the chunk correlates.
	w.ensureResponse(http.StatusOK)
	if w.logBody && len(b) > 0 {
		body := b
		if len(body) > maxBodyCapture {
			body = body[:maxBodyCapture]
		}
		w.h.publishIngressChunk(w.reqID, string(body), "")
	}
	return w.ResponseWriter.Write(b)
}

// ensureResponse emits the ingress.response event exactly once, on the first
// WriteHeader or Write. It records whether the body is capturable text so Write
// knows whether to emit chunks — binary or compressed bodies are streamed to
// the client but not logged, matching the egress proxy.
func (w *streamingWriter) ensureResponse(code int) {
	if w.published {
		return
	}
	w.published = true
	hdr := w.Header()
	w.logBody = isCapturableBody(hdr.Get("Content-Type"), hdr.Get("Content-Encoding"))
	w.h.publishIngressResponse(w.reqID, code, msSince(w.start), flattenHeaders(hdr))
}

func (h *Sandbox) newReverseProxy(c *gin.Context, port, path string) {
	// The in-guest agent's control channels (exec/files/readiness/control) now ride
	// the guest network on fixed TCP ports instead of vsock. They share the guest's
	// netns with the workload and are reachable through this ingress DNAT, so refuse
	// to proxy them — a sandbox user must not be able to drive the agent.
	if isReservedAgentPort(port) {
		c.JSON(http.StatusForbidden, gin.H{"error": "port reserved for the sandbox agent"})
		return
	}
	// The route is registered as a catch-all (/proxy/:port/*path, see
	// api.proxyCatchAllRouter), so gin hands us `path` with a leading slash
	// (e.g. "/json/version"). Trim it so the single `"/" + path` joins below
	// produce one slash, not two.
	path = strings.TrimPrefix(path, "/")
	host := h.proxyHost
	if host == "" {
		host = "127.0.0.1"
	}
	target, err := url.Parse(fmt.Sprintf("http://%s:%s", host, port))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Capture request body before the proxy consumes it.
	reqBody := captureBody(c.Request)

	reqID := h.publishIngressRequest(c, port, path, reqBody)

	// A WebSocket upgrade can't go through the buffering reverse proxy — it
	// hijacks the connection and copies raw bytes, so frames would never be
	// audited. Tunnel it ourselves and record each message as an ingress.chunk,
	// exactly like the egress proxy does for outbound WebSockets.
	if wsaudit.IsUpgrade(c.Request) {
		h.proxyWebSocket(c, target, path, reqID)
		return
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	if rt := h.proxyRoundTripper(); rt != nil {
		rp.Transport = rt
	}
	c.Request.URL.Path = "/" + path
	c.Request.URL.RawPath = "/" + path
	c.Request.Host = target.Host

	sw := &streamingWriter{ResponseWriter: c.Writer, h: h, reqID: reqID, start: time.Now()}
	c.Writer = sw
	rp.ServeHTTP(sw, c.Request)
	// Safety net: if the reverse proxy returned without ever writing a header
	// (it always does for a real response, but a hijack or an error path may
	// not), still emit the response so every request has a paired result.
	sw.ensureResponse(http.StatusOK)
}

// proxyWebSocket tunnels an inbound ws:// upgrade to the exposed port, recording
// one ingress.chunk per application message (labelled "up" for caller→sandbox
// and "down" for sandbox→caller). It mirrors the egress proxy's handleWebSocket:
// strip Sec-WebSocket-Extensions so frames stay uncompressed and auditable, do
// the upstream handshake by hand (no proxy User-Agent injection), then hijack
// the caller connection and pump frames both ways.
func (h *Sandbox) proxyWebSocket(c *gin.Context, target *url.URL, path string, reqID int64) {
	start := time.Now()
	req := c.Request
	req.URL.Path = "/" + path
	req.URL.RawPath = "/" + path
	req.Host = target.Host

	upstream, err := h.proxyDial(req.Context(), target.Host)
	if err != nil {
		h.publishIngressResponse(reqID, http.StatusBadGateway, msSince(start), nil)
		c.String(http.StatusBadGateway, err.Error())
		return
	}
	defer upstream.Close()

	wsaudit.StripExtensions(req.Header)
	if err := wsaudit.WriteUpgradeRequest(upstream, req); err != nil {
		h.publishIngressResponse(reqID, http.StatusBadGateway, msSince(start), nil)
		c.String(http.StatusBadGateway, err.Error())
		return
	}

	upstreamBuf := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamBuf, req)
	if err != nil {
		h.publishIngressResponse(reqID, http.StatusBadGateway, msSince(start), nil)
		c.String(http.StatusBadGateway, err.Error())
		return
	}

	// The workload declined the upgrade (any non-101). Relay its plain HTTP
	// response to the caller and record it as an ordinary result.
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		h.publishIngressResponse(reqID, resp.StatusCode, msSince(start), flattenHeaders(resp.Header))
		for k, vs := range resp.Header {
			for _, v := range vs {
				c.Writer.Header().Add(k, v)
			}
		}
		c.Writer.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(c.Writer, resp.Body)
		return
	}
	resp.Body.Close()

	hj, ok := c.Writer.(http.Hijacker)
	if !ok {
		h.publishIngressResponse(reqID, http.StatusInternalServerError, msSince(start), nil)
		c.String(http.StatusInternalServerError, "no hijacker")
		return
	}
	client, clientBrw, err := hj.Hijack()
	if err != nil {
		h.publishIngressResponse(reqID, http.StatusInternalServerError, msSince(start), nil)
		return
	}
	defer client.Close()

	if err := wsaudit.WriteResponseHeaders(client, resp); err != nil {
		return
	}

	// Response event before the tunnel starts pumping — frame audit events flow
	// as ingress.chunks, so consumers see the 101 immediately, not at close.
	h.publishIngressResponse(reqID, http.StatusSwitchingProtocols, msSince(start), flattenHeaders(resp.Header))
	emit := func(body, label string) { h.publishIngressChunk(reqID, body, label) }
	done := make(chan struct{}, 2)
	go func() {
		wsaudit.Forward(io.MultiReader(clientBrw.Reader, client), upstream, wsaudit.DirUp, target.Host, emit)
		done <- struct{}{}
	}()
	go func() {
		wsaudit.Forward(io.MultiReader(upstreamBuf, upstream), client, wsaudit.DirDown, target.Host, emit)
		done <- struct{}{}
	}()
	<-done
}

// proxyRoundTripper returns the sandbox's shared, keep-alive transport for the
// ingress reverse proxy, building it once. netMark and proxyHost are fixed after
// readiness, so a single transport is safe to reuse across requests — it pools
// connections to the workload instead of dialing (and allocating a Transport) per
// request. Returns nil when no SO_MARK is set, so the proxy falls back to
// http.DefaultTransport (also pooled), preserving the unmarked behavior.
func (h *Sandbox) proxyRoundTripper() http.RoundTripper {
	h.proxyTransportOnce.Do(func() {
		h.proxyTransport = &http.Transport{
			DialContext:           h.proxyDialContext(),
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	})
	return h.proxyTransport
}

// proxyDialContext returns the dial function shared by the reverse-proxy
// transport and the WebSocket tunnel: the SO_MARK dialer when set (bypasses the
// egress REDIRECT), otherwise a plain one, wrapped in holdUntilReady so a
// request that arrives before the workload is listening waits for the port
// instead of failing with a 502.
func (h *Sandbox) proxyDialContext() func(context.Context, string, string) (net.Conn, error) {
	base := markedDialContext(h.netMark)
	if base == nil {
		base = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	return holdUntilReadyDial(base)
}

// proxyDial opens a raw TCP connection to the workload at addr using the same
// marked, hold-until-ready dialer the reverse-proxy transport uses. Used by the
// WebSocket tunnel, which needs the connection directly rather than through an
// http.Transport.
func (h *Sandbox) proxyDial(ctx context.Context, addr string) (net.Conn, error) {
	return h.proxyDialContext()(ctx, "tcp", addr)
}

// holdUntilReadyDial wraps a dial function so a connection to an exposed port
// that isn't accepting connections yet — the workload server is still starting —
// is retried until it succeeds, the request is canceled, or proxyDialHold
// elapses, instead of failing immediately with a 502. A TCP dial to an unbound
// port returns ECONNREFUSED right away (a dial timeout alone never waits), so we
// poll until the server binds the port, then hand back the connection for the
// keep-alive transport to reuse.
func holdUntilReadyDial(
	base func(context.Context, string, string) (net.Conn, error),
) func(context.Context, string, string) (net.Conn, error) {
	const interval = 100 * time.Millisecond
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		deadline := time.Now().Add(proxyDialHold)
		for {
			conn, err := base(ctx, network, addr)
			if err == nil {
				return conn, nil
			}
			// Only hold while the port is refusing connections (not up yet).
			// Any other error, or exceeding the hold window, surfaces normally.
			if !errors.Is(err, syscall.ECONNREFUSED) || time.Now().After(deadline) {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(interval):
			}
		}
	}
}

// isReservedAgentPort reports whether port is one of the in-guest agent's
// host<->guest channels (exec 1024, files 1025, readiness 1026, control 1028),
// which the ingress proxy must never expose to sandbox users. Keep in sync with
// internal/firecracker and cmd/sbxguest.
func isReservedAgentPort(port string) bool {
	switch port {
	case "1024", "1025", "1026", "1028":
		return true
	}
	return false
}

// captureBody reads up to maxBodyCapture bytes from the request body and
// restores it so the reverse proxy can still forward the full body.
func captureBody(r *http.Request) string {
	if r.Body == nil || r.ContentLength == 0 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyCapture+1))
	if err != nil || len(data) == 0 {
		return ""
	}
	// Restore the body for the reverse proxy (full, not truncated).
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), r.Body))
	if len(data) > maxBodyCapture {
		return string(data[:maxBodyCapture])
	}
	return string(data)
}

func (h *Sandbox) publishIngressRequest(c *gin.Context, port, path, body string) int64 {
	method := c.Request.Method
	rawPath := "/" + path
	query := c.Request.URL.RawQuery
	hdrs := flattenHeaders(c.Request.Header)

	return h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		e := gen.IngressRequestEvent{
			Id:        int(id),
			Timestamp: ts,
			Port:      port,
			Method:    method,
			Path:      rawPath,
		}
		if query != "" {
			e.Query = &query
		}
		if len(hdrs) > 0 {
			e.Headers = &hdrs
		}
		if body != "" {
			e.Body = &body
		}
		_ = ev.FromIngressRequestEvent(e)
		return ev
	})
}

// publishIngressResponse emits the ingress.response event with the status,
// headers, and time-to-first-byte. The body is NOT included; it flows as
// ingress.chunk events, mirroring the egress proxy so consumers see long-lived
// SSE / WebSocket streams as they arrive.
func (h *Sandbox) publishIngressResponse(requestID int64, status, durationMs int, headers map[string]string) {
	if status == 0 {
		status = http.StatusOK
	}
	h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		e := gen.IngressResponseEvent{
			Id:         int(id),
			Timestamp:  ts,
			RequestId:  int(requestID),
			Status:     status,
			DurationMs: durationMs,
		}
		if len(headers) > 0 {
			e.Headers = &headers
		}
		_ = ev.FromIngressResponseEvent(e)
		return ev
	})
}

// publishIngressChunk emits one ingress.chunk carrying a slice of the proxied
// response body as it streams back. label is "up"/"down" for WebSocket frame
// directions and empty for HTTP/SSE, matching EgressChunkEvent.
func (h *Sandbox) publishIngressChunk(requestID int64, body, label string) {
	h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		e := gen.IngressChunkEvent{
			Id:        int(id),
			Timestamp: ts,
			RequestId: int(requestID),
			Body:      body,
		}
		if label != "" {
			e.Label = &label
		}
		_ = ev.FromIngressChunkEvent(e)
		return ev
	})
}

// flattenHeaders converts an http.Header into a flat map[string]string, joining
// multi-value headers with ", " per RFC 7230 §3.2.2. Returns nil for an empty
// header set so the audit event omits the field.
func flattenHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	m := make(map[string]string, len(h))
	for k, vs := range h {
		m[k] = strings.Join(vs, ", ")
	}
	return m
}

// msSince returns whole milliseconds elapsed since t.
func msSince(t time.Time) int { return int(time.Since(t).Milliseconds()) }

// isCapturableBody reports whether a response body with the given Content-Type
// and Content-Encoding should be recorded as ingress.chunk events. Only
// uncompressed text is captured; compressed or binary bodies are streamed to
// the caller but not logged, so the audit stream never carries opaque bytes.
func isCapturableBody(contentType, contentEncoding string) bool {
	if enc := strings.ToLower(strings.TrimSpace(contentEncoding)); enc != "" && enc != "identity" {
		return false
	}
	return isTextContentType(contentType)
}

// isTextContentType reports whether ct denotes a human-readable text body worth
// recording. Mirrors the egress proxy's classifier.
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

func (h *Sandbox) ProxyGet(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *Sandbox) ProxyHead(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *Sandbox) ProxyPost(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *Sandbox) ProxyPut(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *Sandbox) ProxyPatch(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *Sandbox) ProxyDelete(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
