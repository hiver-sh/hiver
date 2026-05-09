package api

import (
	"github.com/gin-gonic/gin"
	"github.com/sandbox-platform/agent-sandbox/internal/api/gen"
)

type Handlers struct{}

func NewHandlers() *Handlers {
	return &Handlers{}
}

func (h *Handlers) GetConfig(c *gin.Context) {

}

func (h *Handlers) ApplyConfig(c *gin.Context) {

}

func (h *Handlers) McpStream(c *gin.Context, params gen.McpStreamParams) {

}

func (h *Handlers) McpRequest(c *gin.Context, params gen.McpRequestParams) {

}

func (h *Handlers) GetEvents(c *gin.Context, params gen.GetEventsParams) {

}
