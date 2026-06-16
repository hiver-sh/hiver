package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// Ping reports whether the inner sandbox is ready and (via the server's
// middleware) resets the shutdown timer. Once `ttl` seconds elapse without a
// ping, sandboxd cancels its lifecycle context, which kicks off the same
// graceful-shutdown chain a SIGTERM would (per the /v1/config Ttl description).
//
// Ping bypasses the server's not-ready gate so a readiness probe can poll while
// sandboxd boots: it returns 503 until NotifyReady fires (the workload is up and
// running), then 200. A 200 is the client's signal that exec and other
// workload-facing operations will succeed. Readiness is observed once, off the
// boot path, and broadcast, so keepalive pings are a cheap flag check.
//
// With ?block=true the probe becomes a long-poll: rather than returning 503
// immediately, it waits for readiness, bounded by the request's lifetime (the
// client closing the connection cancels the wait).
func (h *SandboxHandlers) Ping(c *gin.Context, params gen.PingParams) {
	if h.Ready() {
		c.Status(http.StatusOK)
		return
	}
	if params.Block != nil && *params.Block && h.WaitReady(c.Request.Context()) == nil {
		c.Status(http.StatusOK)
		return
	}
	c.JSON(http.StatusServiceUnavailable, gen.Error{Error: "sandbox not ready"})
}
