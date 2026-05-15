package api

import "github.com/gin-gonic/gin"

type ControllerHandlers struct {
}

func NewControllerHandlers() *ControllerHandlers {
	return &ControllerHandlers{}
}

func (h *ControllerHandlers) GetOrCreateSandbox(c *gin.Context, id string) {

}
