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

// SandboxServer is the per-sandbox HTTP API. It can be started before the
// workload exists — it refuses requests (500, or 503 on /v1/ping) until
// NotifyReady fires — so the sandbox binds its port while it boots. Subsystems
// and the entrypoint pty are wired in via the SetX methods as boot creates them.
type SandboxServer struct {
	*http.Server
	handlers *handlers.SandboxHandlers
}

// NewSandboxServer builds the per-sandbox HTTP API. netMark (the reverse-proxy
// dialer's SO_MARK) is a fixed constant known up front; the remaining subsystems
// are injected via the SetX methods as boot creates them.
func NewSandboxServer(port string, netMark int) *SandboxServer {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	hs := handlers.NewSandboxHandlers(netMark)
	r.Use(func(c *gin.Context) {
		if c.FullPath() != "/v1/ping" && !hs.Ready() {
			c.JSON(http.StatusInternalServerError, gen.Error{Error: "sandbox is still starting"})
			c.Abort()
			return
		}
		hs.ResetLifetime()
		c.Next()
	})

	gen.RegisterHandlers(r, hs)

	return &SandboxServer{
		Server: &http.Server{
			Handler: r,
			Addr:    net.JoinHostPort("0.0.0.0", port),
		},
		handlers: hs,
	}
}

// The setters below inject sandboxd's subsystems into the running server as boot
// creates them; the server answers 500 until NotifyReady fires.
func (s *SandboxServer) SetBroker(b *events.Broker)           { s.handlers.SetBroker(b) }
func (s *SandboxServer) SetStore(store *ConfigStore)          { s.handlers.SetStore(store) }
func (s *SandboxServer) SetLifetime(l *Lifetime)              { s.handlers.SetLifetime(l) }
func (s *SandboxServer) SetIsolation(iso isolation.Isolation) { s.handlers.SetIsolation(iso) }

// NotifyReady signals that the inner sandbox is up and running, flipping the
// server from refusing requests to serving them. Called once readiness is
// observed (see cmd/sandboxd).
func (s *SandboxServer) NotifyReady() { s.handlers.NotifyReady() }

// SetEntrypointTTY wires the entrypoint's pty session into the API so
// exec-stream attach requests can reach it. Called once the entrypoint
// launches; backs `/v1/exec-stream` attach requests with an empty command.
func (s *SandboxServer) SetEntrypointTTY(sess *pty.Session) {
	s.handlers.SetEntrypointTTY(sess)
}
