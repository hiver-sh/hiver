package api

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	gen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/sandbox"
	"github.com/sandbox-platform/agent-sandbox/internal/events"
)

type SandboxHandlers struct {
	broker   *events.Broker
	store    *ConfigStore
	lifetime *Lifetime
}

func NewSandboxHandlers(broker *events.Broker, store *ConfigStore, lifetime *Lifetime) *SandboxHandlers {
	return &SandboxHandlers{broker: broker, store: store, lifetime: lifetime}
}

func (h *SandboxHandlers) GetConfig(c *gin.Context) {
	cfg, err := h.store.Get()
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		c.JSON(status, gen.Error{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// ApplyConfig diffs the desired config against the current on-disk
// state, writes the new config, emits a ConfigApplyEvent carrying the
// delta, and returns the post-apply state. Policy enforcers (sbxfuse,
// sbxproxy) subscribe to the event stream and reconcile their in-memory
// rules from the delta — this handler does not call them directly.
func (h *SandboxHandlers) ApplyConfig(c *gin.Context) {
	var desired gen.SandboxConfig
	if err := c.ShouldBindJSON(&desired); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}

	changes, applyErr := h.store.Apply(desired)
	if errors.Is(applyErr, ErrApplyInProgress) {
		c.JSON(http.StatusConflict, gen.Error{Error: applyErr.Error()})
		return
	}

	success := applyErr == nil
	postState := desired
	if !success {
		// Apply rolled back: report the pre-apply state as the post-apply
		// config so callers see the actual on-disk truth.
		if prev, err := h.store.Get(); err == nil {
			postState = prev
		}
	}

	h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		evt := gen.ConfigApplyEvent{
			Id:        int(id),
			Timestamp: ts,
			Success:   success,
			Changes:   changes,
		}
		if applyErr != nil {
			msg := applyErr.Error()
			evt.ErrorMessage = &msg
		}
		_ = ev.FromConfigApplyEvent(evt)
		return ev
	})

	result := gen.ApplyResult{
		Applied: success,
		Config:  postState,
		Changes: changes,
	}
	if applyErr != nil {
		msg := applyErr.Error()
		result.Error = &msg
	}
	c.JSON(http.StatusOK, result)
}

// UploadFile writes a multipart-uploaded file under one of the
// configured FUSE mounts. The `destination` form field must match a
// configured `fs[].mount` exactly; the file lands at
// `<destination>/<basename(filename)>` (the agent-visible path).
//
// The write bypasses the FUSE layer: we open the underlying backend
// directory directly so the per-mount ACLs that gate the agent do
// NOT apply. The API is a higher-privilege control surface than the
// workload — operators seeding inputs over /v1/file should not have
// to grant the agent rw on the same path.
func (h *SandboxHandlers) UploadFile(c *gin.Context) {
	destination := c.PostForm("destination")
	if destination == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "missing form field: destination"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	if !mountConfigured(cfg, destination) {
		c.JSON(http.StatusNotFound, gen.Error{Error: fmt.Sprintf("destination %q does not match any configured mount", destination)})
		return
	}

	header, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}
	name := filepath.Base(header.Filename)
	if name == "." || name == "/" || name == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "invalid file filename"})
		return
	}
	// Resolve to the host-side backend directory so the write lands on
	// real disk and skips sbxfuse. Mirrors spec.FS.BackendPath: backend
	// dir is always `<mount>-backend`, created by sandboxd at startup.
	backendDir := destination + "-backend"
	backendTarget := filepath.Join(backendDir, name)
	agentTarget := filepath.Join(destination, name)

	src, err := header.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	defer src.Close()

	dst, err := os.OpenFile(backendTarget, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	n, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(backendTarget)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: copyErr.Error()})
		return
	}
	if closeErr != nil {
		_ = os.Remove(backendTarget)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: closeErr.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"path": agentTarget, "bytes": n})
}

func mountConfigured(cfg gen.SandboxConfig, dest string) bool {
	for _, fs := range cfg.Fs {
		if FSBase(fs).Mount == dest {
			return true
		}
	}
	return false
}

// GetFile streams a file from beneath one of the configured mounts.
// Like UploadFile, the read is served from the host-side backend
// directory and bypasses sbxfuse — the per-mount ACLs that gate the
// agent do not apply.
func (h *SandboxHandlers) GetFile(c *gin.Context, params gen.GetFileParams) {
	if params.Path == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "missing query parameter: path"})
		return
	}
	cleaned := filepath.Clean(params.Path)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	// Longest-prefix-match on mount roots so /a and /a/b can coexist
	// and `/a/b/x` resolves to the /a/b mount, not /a.
	var matchedMount string
	for _, fs := range cfg.Fs {
		m := FSBase(fs).Mount
		if cleaned == m || strings.HasPrefix(cleaned, strings.TrimRight(m, "/")+"/") {
			if len(m) > len(matchedMount) {
				matchedMount = m
			}
		}
	}
	if matchedMount == "" {
		c.JSON(http.StatusNotFound, gen.Error{Error: fmt.Sprintf("path %q is not under any configured mount", params.Path)})
		return
	}

	// Re-root onto the backend directory. filepath.Clean above already
	// neutralised `..`; re-check the result is contained for defence in
	// depth against any future path-construction change.
	rel := strings.TrimPrefix(cleaned, matchedMount)
	backendDir := matchedMount + "-backend"
	target := filepath.Join(backendDir, rel)
	if target != backendDir && !strings.HasPrefix(target, backendDir+string(filepath.Separator)) {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path escapes the destination mount"})
		return
	}

	info, err := os.Stat(target)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		c.JSON(status, gen.Error{Error: err.Error()})
		return
	}
	if !info.Mode().IsRegular() {
		c.JSON(http.StatusNotFound, gen.Error{Error: "not a regular file"})
		return
	}

	f, err := os.Open(target)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	defer f.Close()

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(cleaned)))
	c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
	c.DataFromReader(http.StatusOK, info.Size(), "application/octet-stream", f, nil)
}

// GetEvents implements the long-lived SSE stream at GET /v1/events.
// Resume semantics: prefer the SSE-standard `Last-Event-ID` header
// (browsers send it automatically on EventSource reconnect); fall back
// to the `lastEventId` query param.
func (h *SandboxHandlers) GetEvents(c *gin.Context, params gen.GetEventsParams) {
	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	after := int64(0)
	if params.LastEventId != nil {
		after = int64(*params.LastEventId)
	}
	if hdr := c.GetHeader("Last-Event-ID"); hdr != "" {
		if parsed, err := strconv.ParseInt(hdr, 10, 64); err == nil {
			after = parsed
		}
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := h.broker.Subscribe(after)
	defer cancel()

	notify := c.Request.Context().Done()
	for {
		select {
		case entry, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSEFrame(w, entry); err != nil {
				return
			}
			flusher.Flush()
		case <-notify:
			return
		}
	}
}

// Ping resets the sandbox shutdown timer. Once `ttl` seconds elapse
// without a ping, sandboxd cancels its lifecycle context, which kicks
// off the same graceful-shutdown chain a SIGTERM would (per the
// /v1/config Ttl description).
func (h *SandboxHandlers) Ping(c *gin.Context) {
	h.lifetime.Reset()
	c.Status(http.StatusOK)
}

// Shutdown signals sandboxd to begin its graceful shutdown immediately
// by self-delivering SIGTERM. We re-enter the same signal-driven path
// SIGTERM-from-outside takes (signal.NotifyContext in main cancels the
// lifecycle context) rather than reaching into an injected cancel func
// — one shutdown cascade, one place to reason about it.
//
// The 200 is written before the kill so the caller observes success;
// the signal is fired from a goroutine after a brief yield to let the
// response flush. In-flight requests started after that point may be
// cut short, which is expected for a shutdown endpoint.
func (h *SandboxHandlers) Shutdown(c *gin.Context) {
	c.Status(http.StatusOK)
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			log.Printf("sandboxd: self-SIGTERM failed: %v", err)
		}
	}()
}

// writeSSEFrame emits a single SSE event:
//
//	id: <int>
//	data: <SandboxEvent JSON>
//	<blank line>
//
// `id:` mirrors the entry id so SSE-aware clients (browsers) resume
// automatically via `Last-Event-ID` on reconnect.
func writeSSEFrame(w io.Writer, entry events.Entry) error {
	body, err := entry.Event.MarshalJSON()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("id: " + strconv.FormatInt(entry.ID, 10) + "\ndata: ")); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n\n"))
	return err
}
