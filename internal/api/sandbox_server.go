package api

import (
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
	"github.com/hiver-sh/hiver/internal/events"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/pty"
)

// NewSandboxServer builds the per-sandbox HTTP API. entrypointTTY is the pty
// wrapping the entrypoint when the config sets tty: true (nil otherwise);
// it backs `/v1/exec-stream` attach requests with an empty command.
func NewSandboxServer(port string, broker *events.Broker, store *ConfigStore, lifetime *Lifetime, iso isolation.Isolation, netMark int, entrypointTTY *pty.Session) *http.Server {
	r := gin.Default()
	r.Use(func(c *gin.Context) {
		// Any request to the API server resets the lifetime of the sandbox.
		lifetime.Reset()
		c.Next()
	})

	hs := handlers.NewSandboxHandlers(broker, store, lifetime, iso, netMark, entrypointTTY)
	gen.RegisterHandlers(r, hs)

	s := &http.Server{
		Handler: r,
		Addr:    net.JoinHostPort("0.0.0.0", port),
	}
	return s
}
