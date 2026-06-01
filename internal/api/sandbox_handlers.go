package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/events"
	"github.com/blasten/hive/internal/spec"
	"github.com/gin-gonic/gin"
)

// agentContainerID is the runc container ID sandboxd always assigns the
// agent. sandboxd runs as PID 1 in the sandbox pod, so os.Getpid() == 1.
const agentContainerID = "agent-1"

type SandboxHandlers struct {
	broker   *events.Broker
	store    *ConfigStore
	lifetime *Lifetime
	upperDir string // host-side path of the overlayfs upper layer
}

func NewSandboxHandlers(broker *events.Broker, store *ConfigStore, lifetime *Lifetime, upperDir string) *SandboxHandlers {
	return &SandboxHandlers{broker: broker, store: store, lifetime: lifetime, upperDir: upperDir}
}

// resolveHostPath maps an agent-visible absolute path to its host-side path.
// FUSE mount backends take priority (longest-prefix match on cfg.Fs); all
// other paths fall back to the overlayfs upper layer so the caller can read
// or write any file the container has touched.
func (h *SandboxHandlers) resolveHostPath(cfg gen.SandboxConfig, cleaned string) string {
	var matchedMount string
	for _, fs := range cfg.Fs {
		m := FSBase(fs).Mount
		if cleaned == m || strings.HasPrefix(cleaned, strings.TrimRight(m, "/")+"/") {
			if len(m) > len(matchedMount) {
				matchedMount = m
			}
		}
	}
	if matchedMount != "" {
		rel := strings.TrimPrefix(cleaned, matchedMount)
		return filepath.Join(matchedMount+spec.BackendSuffix, rel)
	}
	return filepath.Join(h.upperDir, cleaned)
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

	changes, applyErr := h.store.Apply(NormalizeConfig(desired))
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
	cleaned := filepath.Clean(destination)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "destination must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
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

	hostDir := h.resolveHostPath(cfg, cleaned)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	hostTarget := filepath.Join(hostDir, name)
	agentTarget := filepath.Join(cleaned, name)

	src, err := header.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	defer src.Close()

	dst, err := os.OpenFile(hostTarget, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	n, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(hostTarget)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: copyErr.Error()})
		return
	}
	if closeErr != nil {
		_ = os.Remove(hostTarget)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: closeErr.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"path": agentTarget, "bytes": n})
}

// ListDirectory returns the immediate children of a directory. For the
// root path ("/") it lists the overlayfs upper layer so callers see every
// path the container has written to. For any other path the read is served
// from the FUSE mount backend when the path is under a configured mount,
// falling back to the upper layer otherwise. Either way the read bypasses
// sbxfuse ACLs.
func (h *SandboxHandlers) ListDirectory(c *gin.Context, params gen.ListDirectoryParams) {
	path := params.Path
	if path == "" {
		path = "/"
	}
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	target := h.resolveHostPath(cfg, cleaned)

	entries, err := os.ReadDir(target)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		c.JSON(status, gen.Error{Error: err.Error()})
		return
	}

	type dirEntry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	result := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		size := int64(0)
		if !e.IsDir() {
			size = info.Size()
		}
		result = append(result, dirEntry{
			Name:  e.Name(),
			Path:  filepath.Join(cleaned, e.Name()),
			IsDir: e.IsDir(),
			Size:  size,
		})
	}
	c.JSON(http.StatusOK, gin.H{"entries": result})
}

// GetFile streams a file from the sandbox filesystem. The path resolves
// via the same FUSE-backend-first / upper-layer-fallback logic as
// ListDirectory, and bypasses sbxfuse ACLs.
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

	target := h.resolveHostPath(cfg, cleaned)

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

	replay, ch, cancel := h.broker.Subscribe(after)
	defer cancel()

	for _, entry := range replay {
		if err := writeSSEFrame(w, entry); err != nil {
			return
		}
	}
	if len(replay) > 0 {
		flusher.Flush()
	}

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

// Exec runs a shell command inside the agent container and returns the
// complete buffered stdout, stderr, and exit code once the process finishes.
// Each line is also published to the broker as a StdioEvent.
func (h *SandboxHandlers) Exec(c *gin.Context) {
	var req gen.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}

	if err := waitForContainer(c.Request.Context(), agentContainerID); err != nil {
		c.JSON(http.StatusServiceUnavailable, gen.Error{Error: "container not ready: " + err.Error()})
		return
	}

	cmd := exec.CommandContext(c.Request.Context(), "runc", buildRuncExecArgs(req)...)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	var (
		wg                 sync.WaitGroup
		stdoutBuf          strings.Builder
		stderrBuf          strings.Builder
		stdoutMu, stderrMu sync.Mutex
	)
	collect := func(r io.Reader, stream string, buf *strings.Builder, mu *sync.Mutex) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text() + "\n"
			h.publishStdioLine(stream, line)
			mu.Lock()
			buf.WriteString(line)
			mu.Unlock()
		}
	}

	wg.Add(2)
	go collect(stdoutPipe, "stdout", &stdoutBuf, &stdoutMu)
	go collect(stderrPipe, "stderr", &stderrBuf, &stderrMu)
	wg.Wait()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
			return
		}
		exitCode = exitErr.ExitCode()
	}

	c.JSON(http.StatusOK, gin.H{
		"stdout":    stdoutBuf.String(),
		"stderr":    stderrBuf.String(),
		"exit_code": exitCode,
	})
}

// ExecStream runs a shell command inside the agent container and streams
// its output as Server-Sent Events. Each frame has an `event:` field of
// "stdout", "stderr", or "exit"; the final "exit" frame closes the stream.
// Each line is also published to the broker as a StdioEvent.
func (h *SandboxHandlers) ExecStream(c *gin.Context) {
	var req gen.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}

	if err := waitForContainer(c.Request.Context(), agentContainerID); err != nil {
		c.JSON(http.StatusServiceUnavailable, gen.Error{Error: "container not ready: " + err.Error()})
		return
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	cmd := exec.CommandContext(c.Request.Context(), "runc", buildRuncExecArgs(req)...)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var (
		wg    sync.WaitGroup
		sseMu sync.Mutex
	)
	emitSSE := func(eventType, text string) {
		payload, _ := json.Marshal(map[string]string{"type": eventType, "text": text})
		sseMu.Lock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload)
		flusher.Flush()
		sseMu.Unlock()
	}
	pipe := func(r io.Reader, stream string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text() + "\n"
			h.publishStdioLine(stream, line)
			emitSSE(stream, line)
		}
	}

	wg.Add(2)
	go pipe(stdoutPipe, "stdout")
	go pipe(stderrPipe, "stderr")
	wg.Wait()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}

	exitPayload, _ := json.Marshal(map[string]any{"type": "exit", "code": exitCode})
	fmt.Fprintf(w, "event: exit\ndata: %s\n\n", exitPayload)
	flusher.Flush()
}

// publishStdioLine publishes a single line of exec output as a StdioEvent
// to the sandbox broker, mirroring the agent's own stdio stream.
func (h *SandboxHandlers) publishStdioLine(stream, line string) {
	h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		stdio := gen.StdioEvent{Id: int(id), Timestamp: ts}
		if stream == "stdout" {
			stdio.Stdout = &line
		} else {
			stdio.Stderr = &line
		}
		_ = ev.FromStdioEvent(stdio)
		return ev
	})
}

// waitForContainer polls `runc state` until the container is in the "running"
// state or ctx is cancelled. It returns an error if the deadline is exceeded
// or the container reaches a terminal state before running.
func waitForContainer(ctx context.Context, containerID string) error {
	type runcState struct {
		Status string `json:"status"`
	}
	for {
		out, err := exec.CommandContext(ctx, "runc", "state", containerID).Output()
		if err == nil {
			var s runcState
			if json.Unmarshal(out, &s) == nil && s.Status == "running" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// buildRuncExecArgs constructs the argument slice for `runc exec`.
func buildRuncExecArgs(req gen.ExecRequest) []string {
	args := []string{"exec"}
	if req.Cwd != nil && *req.Cwd != "" {
		args = append(args, "--cwd", *req.Cwd)
	}
	args = append(args, agentContainerID, "sh", "-c", req.Command)
	return args
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
