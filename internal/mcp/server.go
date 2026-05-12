package mcp

import (
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	middleware "github.com/oapi-codegen/gin-middleware"
	"github.com/sandbox-platform/agent-sandbox/internal/mcp/gen"
)

func NewServer(port string) *http.Server {
	swagger, err := gen.GetSpec()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading swagger spec: %s", err)
		os.Exit(1)
	}
	swagger.Servers = nil

	r := gin.Default()
	r.Use(allowAllCORS)
	r.Use(middleware.OapiRequestValidator(swagger))

	h := NewHandlers(newMCPServer())
	gen.RegisterHandlers(r, h)

	s := &http.Server{
		Handler: r,
		Addr:    net.JoinHostPort("0.0.0.0", port),
	}
	return s
}

func allowAllCORS(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "*")
	h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
	if c.Request.Method == http.MethodOptions {
		c.AbortWithStatus(http.StatusNoContent)
		return
	}
	c.Next()
}
