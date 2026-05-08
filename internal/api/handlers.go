package api

import "github.com/gin-gonic/gin"

type Handlers struct{}

func NewHandlers() *Handlers {
	return &Handlers{}
}

func (h *Handlers) GetConfig(c *gin.Context) {

}

func (h *Handlers) ApplyConfig(c *gin.Context) {

}

func (h *Handlers) McpStream(c *gin.Context, params McpStreamParams) {

}

func (h *Handlers) McpRequest(c *gin.Context, params McpRequestParams) {

}
