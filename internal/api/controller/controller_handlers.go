package controller

import (
	"context"
	"encoding/json"
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

// packCreator is the optional fast path a runtime can offer for getOrCreate:
// place a sandbox into an already-warm host straight from an in-memory cache,
// with no orchestrator API call. ok=false means no warm host is cached for the
// image, so the handler falls back to the lookup+create path. The k8s runtime
// implements it (cached prewarm hosts); the docker runtime does not.
type packCreator interface {
	tryPackCreate(ctx context.Context, key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, bool, error)
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

	// Fast path: when the runtime can place this sandbox into an already-warm host
	// straight from its in-memory cache (no orchestrator round-trip), do that and
	// return. The POST it issues is idempotent on key, so this also covers a
	// repeated getOrCreate for a sandbox already packed into that host. Falls
	// through to the lookup+create path below when no warm host is cached for the
	// image. ShouldBindBodyWith caches the parsed body, so the slow path can bind
	// it again without re-reading the request.
	//
	// This path intentionally skips the per-key Lookup the slow path does. The one
	// case that loses by it: a key that was cold-booted into a dedicated pod (no
	// warm host existed then), after which a warm host for its image appears, and
	// then getOrCreate is called again for the same key. We pack a fresh sandbox
	// into the warm host instead of returning the existing dedicated one, so for
	// the overlap the key has two sandboxes and the repeat call returns a fresh
	// (empty) one rather than the original's accumulated state. It is self-healing,
	// not a leak: the orphaned dedicated sandbox stops getting /v1/ping, so its
	// inactivity TTL (cmd/sandboxd Lifetime) shuts it down, its pod terminates, and
	// the recycler reaps it (recycler.go). Bounded lifetime ≈ sandbox Ttl +
	// completedPodTTL. Accepted as the cost of keeping the warm path off the API;
	// add a dedicated-pod check here if state continuity across that window matters.
	if pc, ok := h.runtime.(packCreator); ok {
		var cfg sandboxgen.SandboxConfig
		if err := c.ShouldBindBodyWith(&cfg, binding.JSON); err != nil {
			c.JSON(http.StatusBadRequest, sandboxgen.Error{Error: err.Error()})
			return
		}
		sb, packed, err := pc.tryPackCreate(ctx, key, api.NormalizeConfig(cfg))
		if err != nil {
			c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
			return
		}
		if packed {
			log.Printf("sandbox %q: get-or-create (warm host) total %s", key, time.Since(handlerStart).Round(time.Millisecond))
			c.JSON(http.StatusCreated, sb)
			return
		}
	}

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
