package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// maxBodyCapture is the per-direction body capture limit (512 KB).
const maxBodyCapture = 512 * 1024

// recordingWriter wraps gin.ResponseWriter to capture the HTTP status code
// and response body written by the reverse proxy.
type recordingWriter struct {
	gin.ResponseWriter
	status int
	body   bytes.Buffer
	capped bool // true once body exceeded maxBodyCapture
}

func (w *recordingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.capped {
		remaining := maxBodyCapture - w.body.Len()
		if len(b) <= remaining {
			w.body.Write(b)
		} else {
			w.body.Write(b[:remaining])
			w.capped = true
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *recordingWriter) capturedBody() string {
	if w.body.Len() == 0 {
		return ""
	}
	return w.body.String()
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

	rp := httputil.NewSingleHostReverseProxy(target)
	if rt := h.proxyRoundTripper(); rt != nil {
		rp.Transport = rt
	}
	c.Request.URL.Path = "/" + path
	c.Request.URL.RawPath = "/" + path
	c.Request.Host = target.Host

	rw := &recordingWriter{ResponseWriter: c.Writer}
	c.Writer = rw
	start := time.Now()
	rp.ServeHTTP(rw, c.Request)
	durationMs := int(time.Since(start).Milliseconds())

	h.publishIngressResponse(reqID, rw.status, durationMs, rw.capturedBody())
}

// proxyRoundTripper returns the sandbox's shared, keep-alive transport for the
// ingress reverse proxy, building it once. netMark and proxyHost are fixed after
// readiness, so a single transport is safe to reuse across requests — it pools
// connections to the workload instead of dialing (and allocating a Transport) per
// request. Returns nil when no SO_MARK is set, so the proxy falls back to
// http.DefaultTransport (also pooled), preserving the unmarked behavior.
func (h *Sandbox) proxyRoundTripper() http.RoundTripper {
	dialFn := markedDialContext(h.netMark)
	if dialFn == nil {
		return nil
	}
	h.proxyTransportOnce.Do(func() {
		h.proxyTransport = &http.Transport{
			DialContext:           dialFn,
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

	hdrs := make(map[string]string, len(c.Request.Header))
	for k, vs := range c.Request.Header {
		hdrs[k] = strings.Join(vs, ", ")
	}

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

func (h *Sandbox) publishIngressResponse(requestID int64, status, durationMs int, body string) {
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
		if body != "" {
			e.Body = &body
		}
		_ = ev.FromIngressResponseEvent(e)
		return ev
	})
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
