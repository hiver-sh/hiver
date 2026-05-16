package api

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	middleware "github.com/oapi-codegen/gin-middleware"
	gen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/sandbox"
	"github.com/sandbox-platform/agent-sandbox/internal/events"
)

const sandboxProxyPrefix = "/v1/sandbox"

func NewSandboxServer(port string, exposedPort *string,
	proxyTransport http.RoundTripper, broker *events.Broker, store *ConfigStore, lifetime *Lifetime) *http.Server {
	swagger, err := gen.GetSpec()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading swagger spec: %s", err)
		os.Exit(1)
	}
	swagger.Servers = nil

	r := gin.Default()

	if exposedPort != nil {
		h := sandboxProxyHandler(*exposedPort, proxyTransport)
		r.Any(sandboxProxyPrefix, h)
		r.Any(sandboxProxyPrefix+"/*proxyPath", h)
	}

	oapiGroup := r.Group("/")
	oapiGroup.Use(middleware.OapiRequestValidator(swagger))
	hs := NewSandboxHandlers(broker, store, lifetime)
	gen.RegisterHandlers(oapiGroup, hs)

	s := &http.Server{
		Handler: r,
		Addr:    net.JoinHostPort("0.0.0.0", port),
	}
	return s
}

// sandboxProxyHandler forwards /v1/sandbox/<rest> to the sandbox's HTTP
// service at 127.0.0.1:<exposedPort>/<rest>, transparently relaying any
// method, body, status, and headers. FlushInterval=-1 disables response
// buffering so SSE and other streaming responses are delivered in real
// time; Upgrade (WebSocket, h2c) is handled by httputil.ReverseProxy.
func sandboxProxyHandler(port string, transport http.RoundTripper) gin.HandlerFunc {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", port)}
	rp := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.Host = target.Host
			path := strings.TrimPrefix(pr.In.URL.Path, sandboxProxyPrefix)
			if path == "" {
				path = "/"
			}
			pr.Out.URL.Path = path
			// RawPath is recomputed from Path on the outbound request;
			// clearing it avoids carrying the inbound encoded form.
			pr.Out.URL.RawPath = ""
			pr.SetXForwarded()
		},
	}
	return gin.WrapH(rp)
}
