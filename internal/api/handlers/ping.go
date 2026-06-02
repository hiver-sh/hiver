package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Ping resets the sandbox shutdown timer. Once `ttl` seconds elapse
// without a ping, sandboxd cancels its lifecycle context, which kicks
// off the same graceful-shutdown chain a SIGTERM would (per the
// /v1/config Ttl description).
func (h *SandboxHandlers) Ping(c *gin.Context) {
	h.lifetime.Reset()
	c.Status(http.StatusOK)
}
