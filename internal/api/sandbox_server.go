package api

import (
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
)

// SandboxServer is the pod's HTTP API. It can be started before any sandbox
// workload exists — keyed routes answer 404 until their sandbox is registered
// and 500 until it is ready — so the pod binds its port while it boots. The
// per-sandbox subsystems are wired into each handlers.Sandbox by the supervisor
// as boot creates them.
type SandboxServer struct {
	*http.Server
}

// proxyCatchAllRouter wraps the gin router passed to the generated route
// registration so the ingress proxy routes match multi-segment paths.
// oapi-codegen only ever emits a single-segment param for `{path}`
// (/v1/:key/proxy/:port/:path), but a proxied upstream can expose nested paths
// — e.g. Chrome's DevTools/CDP endpoint serves /json/version and
// /devtools/browser/<id>. We rewrite just those registrations to gin's
// catch-all (/v1/:key/proxy/:port/*path) at registration time, leaving every
// other route untouched, instead of hand-editing generated code (which a
// `go generate` would clobber). The catch-all param carries a leading slash;
// the proxy handler normalizes it (see handlers.newReverseProxy).
type proxyCatchAllRouter struct {
	gin.IRouter
}

// rewriteProxyPath turns the generated single-segment proxy pattern into a
// catch-all. Only the proxy routes end in "/proxy/:port/:path"; all others pass
// through unchanged.
func rewriteProxyPath(relativePath string) string {
	const single = "/proxy/:port/:path"
	if strings.HasSuffix(relativePath, single) {
		return strings.TrimSuffix(relativePath, "/:path") + "/*path"
	}
	return relativePath
}

func (r proxyCatchAllRouter) GET(p string, h ...gin.HandlerFunc) gin.IRoutes {
	return r.IRouter.GET(rewriteProxyPath(p), h...)
}
func (r proxyCatchAllRouter) POST(p string, h ...gin.HandlerFunc) gin.IRoutes {
	return r.IRouter.POST(rewriteProxyPath(p), h...)
}
func (r proxyCatchAllRouter) PUT(p string, h ...gin.HandlerFunc) gin.IRoutes {
	return r.IRouter.PUT(rewriteProxyPath(p), h...)
}
func (r proxyCatchAllRouter) PATCH(p string, h ...gin.HandlerFunc) gin.IRoutes {
	return r.IRouter.PATCH(rewriteProxyPath(p), h...)
}
func (r proxyCatchAllRouter) DELETE(p string, h ...gin.HandlerFunc) gin.IRoutes {
	return r.IRouter.DELETE(rewriteProxyPath(p), h...)
}
func (r proxyCatchAllRouter) HEAD(p string, h ...gin.HandlerFunc) gin.IRoutes {
	return r.IRouter.HEAD(rewriteProxyPath(p), h...)
}

// NewSandboxServer builds the pod API over sup: a dispatcher that resolves the
// addressed sandbox by key, fronted by a readiness gate.
func NewSandboxServer(port string, sup handlers.Supervisor) *SandboxServer {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.Use(readyGate(sup))
	gen.RegisterHandlers(proxyCatchAllRouter{r}, handlers.NewSandboxHandlers(sup))

	return &SandboxServer{
		Server: &http.Server{
			Handler: r,
			Addr:    net.JoinHostPort("0.0.0.0", port),
		},
	}
}

// readyGate fronts every request: pod-level routes pass through; keyed routes
// resolve the addressed sandbox, 404 when unknown, and 500 while it is still
// starting. The PUT /v1/{key}/config route is exempt from the readiness check —
// it is how a prewarm sandbox is bootstrapped, so gating it on readiness would
// deadlock the very request that makes the sandbox ready. Create/delete
// (POST/DELETE /v1/{key}) manage sandbox existence themselves.
func readyGate(sup handlers.Supervisor) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.FullPath() {
		case "":
			// No route matched; let gin's 404 handler respond.
			c.Next()
			return
		case "/v1", "/v1/:key", "/v1/events":
			// Pod-level routes: list, create/delete (which manage existence
			// themselves), and the lifecycle event stream (not keyed).
			c.Next()
			return
		}
		key := c.Param("key")
		sb, ok := sup.Sandbox(key)
		if !ok {
			c.JSON(http.StatusNotFound, gen.Error{Error: "sandbox not found: " + key})
			c.Abort()
			return
		}
		// /v1/<key>/ping reports readiness and PUT /v1/<key>/config bootstraps a
		// prewarm sandbox, so both must run before the workload is ready. They
		// still count as activity and reset the keepalive.
		if c.FullPath() == "/v1/:key/ping" ||
			(c.Request.Method == http.MethodPut && c.FullPath() == "/v1/:key/config") {
			sb.ResetLifetime()
			c.Next()
			return
		}
		if !sb.Ready() {
			c.JSON(http.StatusInternalServerError, gen.Error{Error: "sandbox is still starting"})
			c.Abort()
			return
		}
		sb.ResetLifetime()
		c.Next()
	}
}
