package handlers

import (
	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/gin-gonic/gin"
)

func (h *SandboxHandlers) McpRequest(c *gin.Context, _ gen.McpRequestParams) {
	h.mcpHandler.ServeHTTP(c.Writer, c.Request)
}

func (h *SandboxHandlers) McpStream(c *gin.Context, _ gen.McpStreamParams) {
	h.mcpHandler.ServeHTTP(c.Writer, c.Request)
}

func (h *SandboxHandlers) McpDelete(c *gin.Context, _ gen.McpDeleteParams) {
	h.mcpHandler.ServeHTTP(c.Writer, c.Request)
}
