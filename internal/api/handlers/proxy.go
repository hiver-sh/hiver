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

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/gin-gonic/gin"
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

func (h *SandboxHandlers) newReverseProxy(c *gin.Context, port, path string) {
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%s", port))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Capture request body before the proxy consumes it.
	reqBody := captureBody(c.Request)

	reqID := h.publishIngressRequest(c, port, path, reqBody)

	rp := httputil.NewSingleHostReverseProxy(target)
	if dialFn := markedDialContext(h.netMark); dialFn != nil {
		rp.Transport = &http.Transport{DialContext: dialFn}
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

func (h *SandboxHandlers) publishIngressRequest(c *gin.Context, port, path, body string) int64 {
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

func (h *SandboxHandlers) publishIngressResponse(requestID int64, status, durationMs int, body string) {
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

func (h *SandboxHandlers) ProxyGet(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *SandboxHandlers) ProxyHead(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *SandboxHandlers) ProxyPost(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *SandboxHandlers) ProxyPut(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *SandboxHandlers) ProxyPatch(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
func (h *SandboxHandlers) ProxyDelete(c *gin.Context, port, path string) {
	h.newReverseProxy(c, port, path)
}
