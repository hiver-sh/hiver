package handlers

import (
	"net/http"

	"github.com/blasten/hive/internal/runc"
	"github.com/gin-gonic/gin"
)

// GetPorts lists the TCP ports the sandbox exposes — the image's EXPOSE
// directives, read from the image config staged under /mnt. Each is reachable
// through /v1/proxy/{port}/{path}.
func (h *SandboxHandlers) GetPorts(c *gin.Context) {
	cfg, err := runc.ExtractImageConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, cfg.ExposedPorts)
}
