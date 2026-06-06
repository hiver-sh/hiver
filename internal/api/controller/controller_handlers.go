package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/blasten/hive/internal/api"
	gen "github.com/blasten/hive/internal/api/gen/controller"
	sandboxgen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

type ControllerHandlers struct {
	// mu serializes container lifecycle operations so two requests for the
	// same key can't both decide "not running" and race on docker create.
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

// GetOrCreateSandbox is idempotent on key: if a sandbox for key is already
// running its existing endpoint is returned (200); otherwise a new sandbox
// is booted from the request body (201).
func (h *ControllerHandlers) GetOrCreateSandbox(c *gin.Context, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	running, sb, err := h.runtime.Lookup(key)
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

	sb, err = h.runtime.Start(key, api.NormalizeConfig(cfg))
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	c.JSON(http.StatusCreated, sb)
}

// StreamSandboxEvents writes a text/event-stream of sandbox lifecycle events.
func (h *ControllerHandlers) StreamSandboxEvents(c *gin.Context) {
	ch, err := h.runtime.Events(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.SandboxLifecycleEvent{})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, _ := c.Writer.(http.Flusher)
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		case <-c.Request.Context().Done():
			return
		}
	}
}

// ShutdownSandbox stops and removes the sandbox for key. An already-exited
// container is simply removed; a missing sandbox returns 404.
func (h *ControllerHandlers) ShutdownSandbox(c *gin.Context, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.runtime.Shutdown(key); err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			c.JSON(http.StatusNotFound, sandboxgen.Error{Error: fmt.Sprintf("sandbox %q does not exist", key)})
			return
		}
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
