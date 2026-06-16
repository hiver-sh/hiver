package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/hiver-sh/hiver/internal/api"
	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

type ControllerHandlers struct {
	// keys serializes lifecycle operations per sandbox key so two requests for
	// the same key can't both decide "not running" and race on create, while
	// letting different keys proceed concurrently — important now that a create
	// blocks until the sandbox is reachable.
	keys    keyedMutex
	runtime SandboxRuntime
}

// keyedMutex hands out one lock per key, reclaiming a key's lock once no caller
// holds or waits on it so the map can't grow without bound.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refLock
}

type refLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until the caller holds key's lock, returning the unlock func.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*refLock)
	}
	rl := k.locks[key]
	if rl == nil {
		rl = &refLock{}
		k.locks[key] = rl
	}
	rl.refs++
	k.mu.Unlock()

	rl.mu.Lock()
	return func() {
		rl.mu.Unlock()
		k.mu.Lock()
		rl.refs--
		if rl.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
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
	sandboxes, err := h.runtime.List(c.Request.Context())
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
	// handlerStart bounds everything the controller does for this request, so the
	// gap between it and Start's own timing (the lock wait, Lookup, body bind, and
	// response) is attributable; the gap between it and the client-observed time
	// is then pure network/gateway.
	handlerStart := time.Now()
	defer h.keys.lock(key)()

	ctx := c.Request.Context()
	lookupStart := time.Now()
	running, sb, err := h.runtime.Lookup(ctx, key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	log.Printf("sandbox %q: lookup in %s (running=%t)", key, time.Since(lookupStart).Round(time.Millisecond), running)
	if running {
		c.JSON(http.StatusOK, sb)
		return
	}

	var cfg sandboxgen.SandboxConfig
	if err := c.ShouldBindBodyWith(&cfg, binding.JSON); err != nil {
		c.JSON(http.StatusBadRequest, sandboxgen.Error{Error: err.Error()})
		return
	}

	sb, err = h.runtime.Start(ctx, key, api.NormalizeConfig(cfg))
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	log.Printf("sandbox %q: get-or-create handler total %s", key, time.Since(handlerStart).Round(time.Millisecond))
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
	defer h.keys.lock(key)()

	if err := h.runtime.Shutdown(c.Request.Context(), key); err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			c.JSON(http.StatusNotFound, sandboxgen.Error{Error: fmt.Sprintf("sandbox %q does not exist", key)})
			return
		}
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
