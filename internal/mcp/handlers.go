package mcp

import (
	"net/http"

	"github.com/blasten/hive/internal/mcp/gen"
	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Handlers struct {
	mcpHandler *mcp.StreamableHTTPHandler
}

func NewHandlers(mcpServer *mcp.Server) *Handlers {
	return &Handlers{
		mcpHandler: mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return mcpServer },
			nil,
		),
	}
}

func (h *Handlers) GetConfig(c *gin.Context) {
	res := make(map[string]any)
	c.JSON(http.StatusOK, res)
}

func (h *Handlers) ApplyConfig(c *gin.Context) {

}

func (h *Handlers) McpStream(c *gin.Context, _ gen.McpStreamParams) {
	h.mcpHandler.ServeHTTP(c.Writer, c.Request)
}

func (h *Handlers) McpRequest(c *gin.Context, _ gen.McpRequestParams) {
	h.mcpHandler.ServeHTTP(c.Writer, c.Request)
}

func (h *Handlers) McpDelete(c *gin.Context, _ gen.McpDeleteParams) {
	h.mcpHandler.ServeHTTP(c.Writer, c.Request)
}
