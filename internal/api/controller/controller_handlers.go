package controller

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/blasten/hive/internal/api"
	sandboxgen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

type ControllerHandlers struct {
	// mu serializes container lifecycle operations so two requests for the
	// same id can't both decide "not running" and race on docker create.
	mu      sync.Mutex
	runtime SandboxRuntime
}

func NewControllerHandlers() *ControllerHandlers {
	var rt SandboxRuntime
	runtime := os.Getenv("HIVE_RUNTIME")
	if runtime == "k8s" {
		k, err := newK8sRuntime()
		if err != nil {
			log.Fatalf("k8s runtime: %v", err)
		}
		rt = k
	} else {
		rt = newDockerRuntime()
	}
	return &ControllerHandlers{runtime: rt}
}

// ListSandboxes returns all currently running sandboxes.
func (h *ControllerHandlers) ListSandboxes(c *gin.Context) {
	sandboxes, err := h.runtime.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, sandboxes)
}

// GetOrCreateSandbox is idempotent on id: if a sandbox for id is already
// running its existing endpoint is returned (200); otherwise a new sandbox
// is booted from the request body (201).
func (h *ControllerHandlers) GetOrCreateSandbox(c *gin.Context, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	running, sb, err := h.runtime.Lookup(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	if running {
		c.JSON(http.StatusOK, sb)
		return
	}

	var cfg sandboxgen.SandboxConfig
	if err := c.ShouldBindBodyWith(&cfg, binding.JSON); err != nil {
		c.JSON(http.StatusBadRequest, sandboxgen.Error{Error: err.Error()})
		return
	}

	sb, err = h.runtime.Start(id, api.NormalizeConfig(cfg))
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
