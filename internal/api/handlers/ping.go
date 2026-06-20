package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// Ping reports whether this sandbox's workload is ready and (via the ready gate)
// resets its inactivity timer. It is keyed like every other endpoint: the
// addressed sandbox's state is resolved from the key.
//
// Ping bypasses the gate's not-ready check so a readiness probe can poll while
// the workload boots: it returns 503 until the workload is up, then 200. With
// ?block=true it long-polls — waiting for readiness (bounded by the request)
// rather than returning 503 immediately — which is how the controller waits for
// a freshly-started sandbox to become usable.
func (h *Sandbox) Ping(c *gin.Context, params gen.PingParams) {
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
