package api

import (
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	middleware "github.com/oapi-codegen/gin-middleware"
)

func NewServer(port string) *http.Server {
	swagger, err := GetSpec()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading swagger spec: %s", err)
		os.Exit(1)
	}
	r := gin.Default()
	r.Use(middleware.OapiRequestValidator(swagger))

	h := NewHandlers()
	RegisterHandlers(r, h)

	s := &http.Server{
		Handler: r,
		Addr:    net.JoinHostPort("0.0.0.0", port),
	}
	return s
}
