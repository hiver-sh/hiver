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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/events"
	"github.com/blasten/hive/internal/spec"
	"github.com/creack/pty"
	"github.com/gin-gonic/gin"
	"golang.org/x/term"
)

// agentContainerID is the runc container ID sandboxd always assigns the
// agent. sandboxd runs as PID 1 in the sandbox pod, so os.Getpid() == 1.
const agentContainerID = "agent-1"

type SandboxHandlers struct {
	broker    *events.Broker
	store     *ConfigStore
	lifetime  *Lifetime
	upperDir  string   // host-side path of the overlayfs upper layer
	processes sync.Map // id → io.Writer (stdin of a running exec-stream process; pty master for tty sessions)
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

	reqEventID := h.publishExecRequest(req)

	pidPath, err := newExecPIDFile()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: "pid file: " + err.Error()})
		return
	}
	// Guarantee the in-container process tree is reaped if the client aborts
	// (e.g. cancels the request) before the command finishes on its own.
	defer func() {
		if c.Request.Context().Err() != nil {
			killExecTree(pidPath)
		}
		os.Remove(pidPath)
	}()

	cmd := exec.CommandContext(c.Request.Context(), "runc", buildRuncExecArgs(req.Command, req.Cwd, false, req.Env, pidPath)...)
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

	h.publishExecResponse(reqEventID)

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
func (h *SandboxHandlers) ExecStream(c *gin.Context, id string) {
	var req gen.ExecStreamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}
	if err := waitForContainer(c.Request.Context(), agentContainerID); err != nil {
		c.JSON(http.StatusServiceUnavailable, gen.Error{Error: "container not ready: " + err.Error()})
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	// TTY sessions are interactive terminals; their byte stream is not
	// meaningful log output, so the exec request/response and stdio events are
	// published only for the non-tty (pipes) path.
	if req.Tty != nil && *req.Tty {
		h.execStreamTTY(c, id, req, flusher)
	} else {
		h.execStreamPipes(c, id, req, flusher)
	}
}

func (h *SandboxHandlers) execStreamTTY(c *gin.Context, id string, req gen.ExecStreamRequest, flusher http.Flusher) {
	w := c.Writer

	// Interactive runc tty mode: runc allocates the container's pty itself
	// and proxies bytes through its OWN stdio, which must be a terminal. So
	// we hand runc our pty slave as stdin/stdout/stderr and keep the master.
	// runc puts the slave into raw mode and relays to the in-container pty,
	// giving the process a real controlling terminal (isatty() true inside).
	//
	// --console-socket is NOT used: runc rejects it unless --detach is also
	// set ("cannot use console socket if runc will not detach or allocate
	// tty"), and detaching would forfeit the exec exit code from cmd.Wait().
	master, slave, err := pty.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: "open pty: " + err.Error()})
		return
	}

	// The outer pty is pure transport between sandboxd and runc; put it in
	// raw mode so it never echoes or line-edits. The in-container pty that
	// runc allocates is the real terminal the process drives. Without this,
	// input written before runc/the program switch the line discipline to
	// raw gets echoed back here on top of the program's own echo.
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		master.Close()
		slave.Close()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: "set raw: " + err.Error()})
		return
	}

	pidPath, err := newExecPIDFile()
	if err != nil {
		master.Close()
		slave.Close()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: "pid file: " + err.Error()})
		return
	}

	cmd := exec.CommandContext(c.Request.Context(), "runc", buildRuncExecArgs(req.Command, req.Cwd, true, req.Env, pidPath)...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Make the outer pty runc's controlling terminal (Setsid makes runc a
	// session leader; Setctty adopts fd 0 — the slave — as its controlling tty).
	// Without this the kernel never delivers SIGWINCH to runc when we resize the
	// master, so window-size changes never reach the in-container pty and the
	// program is stuck at the startup default size (stale content survives clears).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		os.Remove(pidPath)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	slave.Close() // runc dup'd it at exec; the parent no longer needs it.

	// Gate stdin until the program is up: client writes that arrive while the
	// program is still starting are buffered and flushed once it produces its
	// first output (a prompt etc.), so they reach a program that has already
	// switched the terminal to raw mode instead of being echoed as type-ahead.
	// A timeout flushes anyway for programs that read before printing.
	stdin := &gatedWriter{w: master}
	h.processes.Store(id, &ptyProcess{stdin: stdin, master: master})
	defer func() {
		h.processes.Delete(id)
		master.Close()
		// On client abort the in-container process may still be running and
		// orphaned; kill the whole tree. On normal exit it has already gone.
		if c.Request.Context().Err() != nil {
			killExecTree(pidPath)
		}
		os.Remove(pidPath)
	}()
	relTimer := time.AfterFunc(2*time.Second, func() { _ = stdin.release() })
	defer relTimer.Stop()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emitSSE := func(eventType, text string) {
		payload, _ := json.Marshal(map[string]string{"type": eventType, "text": text})
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload)
		flusher.Flush()
	}

	buf := make([]byte, 4096)
	for {
		n, readErr := master.Read(buf)
		if n > 0 {
			_ = stdin.release() // program produced output → safe to deliver stdin
			emitSSE("stdout", string(buf[:n]))
		}
		if readErr != nil {
			break
		}
	}

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

func (h *SandboxHandlers) execStreamPipes(c *gin.Context, id string, req gen.ExecStreamRequest, flusher http.Flusher) {
	reqEventID := h.publishExecRequest(gen.ExecRequest{Command: req.Command, Cwd: req.Cwd})
	w := c.Writer

	pidPath, err := newExecPIDFile()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: "pid file: " + err.Error()})
		return
	}

	cmd := exec.CommandContext(c.Request.Context(), "runc", buildRuncExecArgs(req.Command, req.Cwd, false, req.Env, pidPath)...)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		os.Remove(pidPath)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		os.Remove(pidPath)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		os.Remove(pidPath)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		os.Remove(pidPath)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	h.processes.Store(id, stdinPipe)
	defer func() {
		h.processes.Delete(id)
		stdinPipe.Close()
		// On client abort the in-container process may still be running and
		// orphaned; kill the whole tree. On normal exit it has already gone.
		if c.Request.Context().Err() != nil {
			killExecTree(pidPath)
		}
		os.Remove(pidPath)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
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

	h.publishExecResponse(reqEventID)

	exitPayload, _ := json.Marshal(map[string]any{"type": "exit", "code": exitCode})
	fmt.Fprintf(w, "event: exit\ndata: %s\n\n", exitPayload)
	flusher.Flush()
}

func (h *SandboxHandlers) ExecStreamStdin(c *gin.Context, id string) {
	val, ok := h.processes.Load(id)
	if !ok {
		c.JSON(http.StatusNotFound, gen.Error{Error: "no running process with id: " + id})
		return
	}
	var body gen.ExecStreamStdinJSONRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}

	// CSI 8 ; rows ; cols t — XTWINOPS resize sequence sent by the client on
	// terminal resize. Intercept it to set the PTY window size instead of
	// forwarding it to the shell.
	if p, ok := val.(*ptyProcess); ok {
		var rows, cols uint16
		if n, _ := fmt.Sscanf(body.Data, "\x1b[8;%d;%dt", &rows, &cols); n == 2 {
			pty.Setsize(p.master, &pty.Winsize{Rows: rows, Cols: cols})
			c.Status(http.StatusNoContent)
			return
		}
		if _, err := io.WriteString(p.stdin, body.Data); err != nil {
			c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
			return
		}
	} else {
		if _, err := io.WriteString(val.(io.Writer), body.Data); err != nil {
			c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
			return
		}
	}
	c.Status(http.StatusNoContent)
}

// ptyProcess bundles the stdin writer and PTY master for a TTY exec-stream
// session so ExecStreamStdin can both write input and resize the terminal.
type ptyProcess struct {
	stdin  io.Writer
	master *os.File
}

// gatedWriter buffers writes until release, then flushes them in order and
// lets subsequent writes pass through to w. It gates client stdin against a
// pty whose program is still starting: input written before the program
// switches the terminal to raw mode would otherwise be echoed as type-ahead.
type gatedWriter struct {
	mu       sync.Mutex
	w        io.Writer
	released bool
	buf      []byte
}

func (g *gatedWriter) Write(b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.released {
		g.buf = append(g.buf, b...)
		return len(b), nil
	}
	return g.w.Write(b)
}

// release flushes any buffered input and opens the gate. Idempotent.
func (g *gatedWriter) release() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return nil
	}
	g.released = true
	if len(g.buf) == 0 {
		return nil
	}
	_, err := g.w.Write(g.buf)
	g.buf = nil
	return err
}

// publishExecRequest publishes an ExecRequestEvent and returns its assigned id.
func (h *SandboxHandlers) publishExecRequest(req gen.ExecRequest) int64 {
	cwd := "/"
	if req.Cwd != nil && *req.Cwd != "" {
		cwd = *req.Cwd
	}
	return h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromExecRequestEvent(gen.ExecRequestEvent{
			Id:        int(id),
			Timestamp: ts,
			Cwd:       cwd,
			Command:   req.Command,
		})
		return ev
	})
}

// publishExecResponse publishes an ExecResponseEvent correlated to the given request id.
func (h *SandboxHandlers) publishExecResponse(requestID int64) {
	h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromExecResponseEvent(gen.ExecResponseEvent{
			Id:        int(id),
			Timestamp: ts,
			RequestId: int(requestID),
		})
		return ev
	})
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
// When tty is set, --tty puts runc in interactive terminal mode (it proxies
// the container pty through its own stdio, which the caller supplies as a
// pty slave).
//
// env entries are passed as `--env KEY=VALUE` flags. runc seeds the exec
// process with the container's configured environment (i.e. the sandbox
// config's `env`) and merges these on top, so callers that omit env inherit
// the sandbox config environment unchanged.
//
// pidFile, when set, becomes `--pid-file`: runc writes the host-namespace PID
// of the spawned process there so the caller can kill the whole process tree
// on teardown (SIGKILL of the runc process alone does not reliably reap the
// in-container workload).
func buildRuncExecArgs(command string, cwd *string, tty bool, env *map[string]string, pidFile string) []string {
	args := []string{"exec"}
	if tty {
		args = append(args, "--tty")
	}
	if cwd != nil && *cwd != "" {
		args = append(args, "--cwd", *cwd)
	}
	if pidFile != "" {
		args = append(args, "--pid-file", pidFile)
	}
	if env != nil {
		// Sort keys so the flag order is deterministic.
		keys := make([]string, 0, len(*env))
		for k := range *env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--env", k+"="+(*env)[k])
		}
	}
	args = append(args, agentContainerID, "sh", "-c", command)
	return args
}

// newExecPIDFile creates an empty temp file for `runc exec --pid-file`. runc
// overwrites it with the spawned process's PID. The caller is responsible for
// removing it.
func newExecPIDFile() (string, error) {
	f, err := os.CreateTemp("", "hive-exec-*.pid")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	return name, nil
}

// killExecTree reads the PID runc wrote to pidPath and SIGKILLs that process
// together with every descendant. Killing the runc process does not reliably
// reap the in-container workload (runc sets no parent-death signal for exec'd
// processes), so we kill the tree explicitly to guarantee teardown.
//
// Call this only on an aborted exec (client disconnect): on normal completion
// the process has already exited and its PID could have been recycled by an
// unrelated process.
func killExecTree(pidPath string) {
	pid, ok := readExecPID(pidPath)
	if !ok {
		return
	}
	killProcessTree(pid)
}

// readExecPID reads and parses the PID runc wrote to pidPath. runc writes the
// file right after spawning the process, so on a very early abort it may not
// exist yet; retry briefly to cover that window.
func readExecPID(pidPath string) (int, bool) {
	for i := 0; i < 10; i++ {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
				return pid, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, false
}

// killProcessTree SIGKILLs rootPID and all of its descendants. It snapshots
// the parent→child relationships from /proc first and then signals every
// member of the subtree, so descendants survive being re-parented to the
// container's init (which a naive live parent-walk would lose) and are still
// killed. PIDs are interpreted in sandboxd's PID namespace, which is where
// runc reports them and where /proc lists the in-container processes.
func killProcessTree(rootPID int) {
	if rootPID <= 1 {
		return
	}
	children := map[int][]int{}
	if entries, err := os.ReadDir("/proc"); err == nil {
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			if ppid, ok := readPPID(pid); ok {
				children[ppid] = append(children[ppid], pid)
			}
		}
	}

	// Breadth-first collection of the whole subtree before signaling anything.
	victims := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for i := 0; i < len(victims); i++ {
		for _, c := range children[victims[i]] {
			if !seen[c] {
				seen[c] = true
				victims = append(victims, c)
			}
		}
	}
	for _, pid := range victims {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// readPPID returns the parent PID from /proc/<pid>/stat.
func readPPID(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parsePPIDStat(string(data))
}

// parsePPIDStat extracts the parent PID (the 4th field) from the contents of
// /proc/<pid>/stat. The comm field (2nd) is wrapped in parentheses and may
// itself contain spaces and parentheses, so the remaining space-separated
// fields are parsed after the final ')'.
func parsePPIDStat(s string) (int, bool) {
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+2 > len(s) {
		return 0, false
	}
	fields := strings.Fields(s[rparen+2:]) // state, ppid, ...
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
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
