package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// ErrApplyInProgress reports that a previous ApplyConfig call is still
// running; the handler translates this to HTTP 409.
var ErrApplyInProgress = errors.New("a previous apply is still in progress")

// Supervisor create/delete errors the dispatcher maps to HTTP status codes.
var (
	// ErrSandboxNotFound is returned by Delete when no sandbox exists for the key.
	ErrSandboxNotFound = errors.New("sandbox not found")
	// ErrPodOccupied is returned by Create when the pod can't host the key (a
	// defensive guard; a pack host accepts new keys).
	ErrPodOccupied = errors.New("pod already hosts its sandbox")
)

type configStore interface {
	Get() (gen.SandboxConfig, error)
	Apply(gen.SandboxConfig) (gen.Changes, error)
}

type lifetime interface {
	Reset()
}

// Supervisor is the pod-level owner of the sandbox map and the shared sidecars.
// It is implemented by cmd/sandboxd; the handlers depend only on this interface
// so the API package stays free of the runtime/isolation wiring.
type Supervisor interface {
	// Sandbox resolves a running sandbox by key.
	Sandbox(key string) (*Sandbox, bool)
	// Create boots a sandbox for key from cfg (the pod's fixed image). Returns
	// ErrPodOccupied when a sandbox for a new key cannot yet be allocated.
	Create(ctx context.Context, key string, cfg gen.SandboxConfig) (*Sandbox, error)
	// Delete tears the sandbox for key down. Returns ErrSandboxNotFound if absent.
	Delete(ctx context.Context, key string) error
	// List returns the sandboxes currently managed by the pod.
	List() []*Sandbox
	// SubscribeLifecycle streams inner-sandbox lifecycle transitions for the
	// pod-level GET /v1/events stream. The returned func unsubscribes.
	SubscribeLifecycle() (events <-chan gen.PodEvent, cancel func())
	// RoutingID is this pod's sandbox routing id: its IPv4 address packed into a
	// UUID (podid.FromIP). CreateSandbox echoes it so the response matches the
	// controller's getOrCreateSandbox payload — in Kubernetes creates route
	// straight to the pod, so the client reads this body. uuid.Nil when the pod
	// IP is unknown (e.g. the Docker runtime, where the controller assigns the id
	// and ignores this body).
	RoutingID() openapi_types.UUID
}

// SandboxHandlers implements the generated ServerInterface as a thin dispatcher:
// keyed routes resolve the addressed *Sandbox and delegate to its method;
// pod-level routes (list, ping, create, delete) act on the supervisor directly.
type SandboxHandlers struct {
	sup Supervisor
}

// NewSandboxHandlers builds the dispatcher over the pod's supervisor.
func NewSandboxHandlers(sup Supervisor) *SandboxHandlers {
	return &SandboxHandlers{sup: sup}
}

// resolve looks up the sandbox for key, writing a 404 and returning false when
// it is unknown.
func (h *SandboxHandlers) resolve(c *gin.Context, key string) (*Sandbox, bool) {
	sb, ok := h.sup.Sandbox(key)
	if !ok {
		c.JSON(http.StatusNotFound, gen.Error{Error: "sandbox not found: " + key})
		return nil, false
	}
	return sb, true
}

// ListSandboxes returns the sandboxes managed by the pod.
func (h *SandboxHandlers) ListSandboxes(c *gin.Context) {
	list := h.sup.List()
	out := gen.SandboxList{Sandboxes: make([]gen.SandboxSummary, 0, len(list))}
	for _, sb := range list {
		out.Sandboxes = append(out.Sandboxes, gen.SandboxSummary{Key: sb.Key(), Ready: sb.Ready(), Status: sb.Status()})
	}
	c.JSON(http.StatusOK, out)
}

// StreamPodEvents opens the pod-level SSE stream of inner-sandbox lifecycle
// transitions (one frame per PodEvent). The controller holds one of these open
// per pod to surface inner sandboxes on its own lifecycle stream.
func (h *SandboxHandlers) StreamPodEvents(c *gin.Context) {
	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	// Subscribe before snapshotting so a transition that races the snapshot is
	// buffered and delivered after (at worst a duplicate, which is idempotent).
	events, cancel := h.sup.SubscribeLifecycle()
	defer cancel()

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Snapshot the current sandboxes so a freshly-connected subscriber (the
	// controller, which connects to a pod after it has already created sandboxes)
	// learns the existing set immediately; it then receives only transitions.
	for _, sb := range h.sup.List() {
		snap := gen.PodEvent{
			Key:       sb.Key(),
			Status:    gen.PodEventStatus(sb.Status()),
			Timestamp: time.Now(),
		}
		if data, err := json.Marshal(snap); err == nil {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
		}
	}
	flusher.Flush()

	notify := c.Request.Context().Done()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-notify:
			return
		}
	}
}

// CreateSandbox brings up the sandbox for key, returning the existing one (200)
// if it is already running or a newly-created one (201). The configuration is the
// JSON request body; an empty/absent body yields a default config.
func (h *SandboxHandlers) CreateSandbox(c *gin.Context, key gen.Key) {
	if sb, ok := h.sup.Sandbox(key); ok {
		ref := h.sandboxRef(sb)
		c.Header("x-hiver-sandbox-id", ref.Id.String())
		c.Header("x-hiver-sandbox-key", ref.Key)
		c.JSON(http.StatusOK, ref)
		return
	}

	// The body is the config. Bind it when present, but tolerate an empty/absent
	// body (defaults apply).
	var cfg gen.SandboxConfig
	_ = c.ShouldBindJSON(&cfg)

	sb, err := h.sup.Create(c.Request.Context(), key, cfg)
	if err != nil {
		switch {
		case errors.Is(err, ErrPodOccupied):
			c.JSON(http.StatusConflict, gen.Error{Error: err.Error()})
		default:
			// Log server-side: the error is returned in the body, but the controller
			// (packCreateSandbox) only records "status 500", so without this the real
			// cause never reaches the pod log.
			log.Printf("sandboxd: create %q: %v", key, err)
			c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		}
		return
	}
	ref := h.sandboxRef(sb)
	// Mirror the controller's GetOrCreateSandbox (controller_handlers.go): the
	// inspector's nested-sandbox detection (relayLinkedSandboxEvents.ts) watches
	// egress.response events for these headers to discover sandboxes spawned by
	// another sandbox. Creates routed straight to this pod (Kubernetes/microVM,
	// see sandboxRef's doc comment) bypass the controller entirely, so without
	// this the headers never reach that egress.response event and detection
	// silently fails on that backend.
	c.Header("x-hiver-sandbox-id", ref.Id.String())
	c.Header("x-hiver-sandbox-key", ref.Key)
	c.JSON(http.StatusCreated, ref)
}

// sandboxRef builds the create response: the host pod's routing id, the sandbox
// key, and its current lifecycle status. It mirrors the controller's Sandbox so
// the client observes the same shape whether the create went through the
// controller (Docker) or straight to the pod (Kubernetes, where the gateway
// rewrites POST /v1/sandboxes/{key} to this handler).
func (h *SandboxHandlers) sandboxRef(sb *Sandbox) gen.Sandbox {
	status := sb.Status()
	return gen.Sandbox{Id: h.sup.RoutingID(), Key: sb.Key(), Status: &status}
}

// DeleteSandbox tears the sandbox for key down.
func (h *SandboxHandlers) DeleteSandbox(c *gin.Context, key gen.Key) {
	if err := h.sup.Delete(c.Request.Context(), key); err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			c.JSON(http.StatusNotFound, gen.Error{Error: "sandbox not found: " + key})
			return
		}
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *SandboxHandlers) Ping(c *gin.Context, key gen.Key, params gen.PingParams) {
	if sb, ok := h.resolve(c, key); ok {
		sb.Ping(c, params)
	}
}

func (h *SandboxHandlers) GetConfig(c *gin.Context, key gen.Key) {
	if sb, ok := h.resolve(c, key); ok {
		sb.GetConfig(c)
	}
}

func (h *SandboxHandlers) ApplyConfig(c *gin.Context, key gen.Key) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ApplyConfig(c)
	}
}

func (h *SandboxHandlers) GetInfo(c *gin.Context, key gen.Key) {
	if sb, ok := h.resolve(c, key); ok {
		sb.GetInfo(c)
	}
}

func (h *SandboxHandlers) Snapshot(c *gin.Context, key gen.Key) {
	if sb, ok := h.resolve(c, key); ok {
		sb.Snapshot(c)
	}
}

func (h *SandboxHandlers) GetEvents(c *gin.Context, key gen.Key, params gen.GetEventsParams) {
	if sb, ok := h.resolve(c, key); ok {
		sb.GetEvents(c, params)
	}
}

func (h *SandboxHandlers) Exec(c *gin.Context, key gen.Key) {
	if sb, ok := h.resolve(c, key); ok {
		sb.Exec(c)
	}
}

func (h *SandboxHandlers) ExecStream(c *gin.Context, key gen.Key, id string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ExecStream(c, id)
	}
}

func (h *SandboxHandlers) ExecStreamStdin(c *gin.Context, key gen.Key, id string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ExecStreamStdin(c, id)
	}
}

func (h *SandboxHandlers) UploadFile(c *gin.Context, key gen.Key, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.UploadFile(c, path)
	}
}

func (h *SandboxHandlers) ListDirectory(c *gin.Context, key gen.Key, params gen.ListDirectoryParams) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ListDirectory(c, params)
	}
}

func (h *SandboxHandlers) GetFile(c *gin.Context, key gen.Key, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.GetFile(c, path)
	}
}

func (h *SandboxHandlers) DeleteFile(c *gin.Context, key gen.Key, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.DeleteFile(c, path)
	}
}

func (h *SandboxHandlers) GetPorts(c *gin.Context, key gen.Key) {
	if sb, ok := h.resolve(c, key); ok {
		sb.GetPorts(c)
	}
}

func (h *SandboxHandlers) ProxyGet(c *gin.Context, key gen.Key, port, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ProxyGet(c, port, path)
	}
}
func (h *SandboxHandlers) ProxyHead(c *gin.Context, key gen.Key, port, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ProxyHead(c, port, path)
	}
}
func (h *SandboxHandlers) ProxyPost(c *gin.Context, key gen.Key, port, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ProxyPost(c, port, path)
	}
}
func (h *SandboxHandlers) ProxyPut(c *gin.Context, key gen.Key, port, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ProxyPut(c, port, path)
	}
}
func (h *SandboxHandlers) ProxyPatch(c *gin.Context, key gen.Key, port, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ProxyPatch(c, port, path)
	}
}
func (h *SandboxHandlers) ProxyDelete(c *gin.Context, key gen.Key, port, path string) {
	if sb, ok := h.resolve(c, key); ok {
		sb.ProxyDelete(c, port, path)
	}
}

// fsBase decodes the variant-agnostic fields (mount, backend, acls)
// shared by every FileSystem oneOf member.
func fsBase(fs gen.FileSystem) gen.FileSystemBase {
	var base gen.FileSystemBase
	if b, err := fs.MarshalJSON(); err == nil {
		_ = json.Unmarshal(b, &base)
	}
	return base
}

// normalizeConfig fills in default values for fields the server enforces
// when absent.
func normalizeConfig(cfg gen.SandboxConfig) gen.SandboxConfig {
	for i, fs := range cfg.Fs {
		base := fsBase(fs)
		if base.Acls != nil && len(*base.Acls) > 0 {
			continue
		}
		acls := &[]gen.ACLRule{{Path: base.Mount + "/**", Access: gen.ACLRuleAccessRw}}
		switch base.Backend {
		case gen.BackendLocal:
			if v, err := fs.AsLocalFileSystem(); err == nil {
				v.Acls = acls
				_ = cfg.Fs[i].FromLocalFileSystem(v)
			}
		case gen.BackendGdrive:
			if v, err := fs.AsGDriveFileSystem(); err == nil {
				v.Acls = acls
				_ = cfg.Fs[i].FromGDriveFileSystem(v)
			}
		case gen.BackendGcs:
			if v, err := fs.AsGCSFileSystem(); err == nil {
				v.Acls = acls
				_ = cfg.Fs[i].FromGCSFileSystem(v)
			}
		case gen.BackendS3:
			if v, err := fs.AsS3FileSystem(); err == nil {
				v.Acls = acls
				_ = cfg.Fs[i].FromS3FileSystem(v)
			}
		case gen.BackendAzure:
			if v, err := fs.AsAzureBlobFileSystem(); err == nil {
				v.Acls = acls
				_ = cfg.Fs[i].FromAzureBlobFileSystem(v)
			}
		case gen.BackendOnedrive:
			if v, err := fs.AsOneDriveFileSystem(); err == nil {
				v.Acls = acls
				_ = cfg.Fs[i].FromOneDriveFileSystem(v)
			}
		case gen.BackendExternal:
			if v, err := fs.AsExternalFileSystem(); err == nil {
				v.Acls = acls
				_ = cfg.Fs[i].FromExternalFileSystem(v)
			}
		}
	}
	return cfg
}
