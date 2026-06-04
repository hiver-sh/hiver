package api

import (
	"fmt"
	"net"
	"net/http"
	"os"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/api/handlers"
	"github.com/blasten/hive/internal/events"
	"github.com/blasten/hive/internal/isolation"
	"github.com/gin-gonic/gin"
	middleware "github.com/oapi-codegen/gin-middleware"
)

func NewSandboxServer(port string, broker *events.Broker, store *ConfigStore, lifetime *Lifetime, iso isolation.Isolation, netMark int) *http.Server {
	swagger, err := gen.GetSpec()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading swagger spec: %s", err)
		os.Exit(1)
	}
	swagger.Servers = nil

	r := gin.Default()

	oapiGroup := r.Group("/")
	oapiGroup.Use(middleware.OapiRequestValidator(swagger))
	hs := handlers.NewSandboxHandlers(broker, store, lifetime, iso, netMark)
	gen.RegisterHandlers(oapiGroup, hs)

	s := &http.Server{
		Handler: r,
		Addr:    net.JoinHostPort("0.0.0.0", port),
	}
	return s
}
