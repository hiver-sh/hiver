package controller

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	gen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/controller"
	sandboxgen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/sandbox"
)

type ControllerHandlers struct {
	// mu serializes container lifecycle operations so two requests for the
	// same id can't both decide "not running" and race on docker create.
	mu      sync.Mutex
	runtime SandboxRuntime
}

func NewControllerHandlers() *ControllerHandlers {
	return &ControllerHandlers{
		runtime: newDockerRuntime(),
	}
}

// GetOrCreateSandbox is idempotent on id: if a sandbox for id is already
// running its existing endpoint is returned (200); otherwise a new sandbox
// is booted from the request body (201).
func (h *ControllerHandlers) GetOrCreateSandbox(c *gin.Context, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	running, endpoint, err := h.runtime.Lookup(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	if running {
		c.JSON(http.StatusOK, gen.Sandbox{Id: id, Endpoint: endpoint})
		return
	}

	var cfg sandboxgen.SandboxConfig
	if err := c.ShouldBindBodyWith(&cfg, binding.JSON); err != nil {
		c.JSON(http.StatusBadRequest, sandboxgen.Error{Error: err.Error()})
		return
	}

	sb, err := h.runtime.Start(id, cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	c.JSON(http.StatusCreated, sb)
}

// ShutdownSandbox stops and removes the sandbox for id. An already-exited
// container is simply removed; a missing sandbox returns 404.
func (h *ControllerHandlers) ShutdownSandbox(c *gin.Context, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.runtime.Shutdown(id); err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			c.JSON(http.StatusNotFound, sandboxgen.Error{Error: fmt.Sprintf("sandbox %q does not exist", id)})
			return
		}
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
