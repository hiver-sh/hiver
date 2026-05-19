package controller

import (
	"fmt"
	"net"
	"net/http"
	"os"

	gen "github.com/blasten/hive/internal/api/gen/controller"
	"github.com/gin-gonic/gin"
	middleware "github.com/oapi-codegen/gin-middleware"
)

func NewControllerServer(port string) *http.Server {
	swagger, err := gen.GetSpec()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading swagger spec: %s", err)
		os.Exit(1)
	}
	swagger.Servers = nil

	r := gin.Default()
	r.Use(middleware.OapiRequestValidator(swagger))

	h := NewControllerHandlers()
	gen.RegisterHandlers(r, h)

	s := &http.Server{
		Handler: r,
		Addr:    net.JoinHostPort("0.0.0.0", port),
	}
	return s
}
