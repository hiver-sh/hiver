package controller

import (
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
)

func NewControllerServer(port string) *http.Server {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	h := NewControllerHandlers()
	gen.RegisterHandlers(r, h)

	s := &http.Server{
		Handler: r,
		Addr:    net.JoinHostPort("0.0.0.0", port),
	}
	return s
}
