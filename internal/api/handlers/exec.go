package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/pty"
)

// ttyStdin is the control surface a TTY-backed exec stream registers in
// h.processes: stdin writes plus terminal resizes. Both an interactive
// `exec-stream` (a per-request *tty.Session) and the entrypoint attach (the
// shared entrypoint *tty.Session) satisfy it; the non-tty pipes path registers
// a plain io.Writer (the process's stdin pipe) instead.
type ttyStdin interface {
	io.Writer
	Resize(rows, cols uint16) error
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

	reqEventID := h.publishExecRequest(req)

	stdout, stderr, exitCode, err := h.execCommand(c.Request.Context(), req.Command, req.Cwd, req.Env)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gen.Error{Error: err.Error()})
		return
	}

	h.publishExecResponse(reqEventID)

	c.JSON(http.StatusOK, gin.H{
		"stdout":    stdout,
		"stderr":    stderr,
		"exit_code": exitCode,
	})
}

// ExecStream runs a shell command inside the agent container and streams
// its output as Server-Sent Events. Each frame has an `event:` field of
// "stdout", "stderr", or "exit"; the final "exit" frame closes the stream.
//
// When the request command is empty, the stream instead attaches to the
// sandbox entrypoint's TTY (see execStreamAttach).
func (h *SandboxHandlers) ExecStream(c *gin.Context, id string) {
	var req gen.ExecStreamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	// An empty command attaches to the entrypoint TTY rather than running a
	// new command. No WaitReady: the entrypoint session only exists once the
	// agent has started, so reaching execStreamAttach already implies it.
	if req.Command == nil || *req.Command == "" {
		h.execStreamAttach(c, id, flusher)
		return
	}
	if err := h.iso.WaitReady(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gen.Error{Error: "container not ready: " + err.Error()})
		return
	}
	// TTY sessions are interactive terminals; their byte stream is not
	// meaningful log output, so the exec request/response and stdio events are
	// published only for the non-tty (pipes) path.
	if req.Tty != nil && *req.Tty {
		h.execStreamTTY(c, id, *req.Command, req, flusher)
	} else {
		h.execStreamPipes(c, id, req, flusher)
	}
}

// execStreamTTY starts command in a fresh pty and streams its terminal output
// as SSE. The session ends when the process exits; the final exit frame
// carries its exit code.
func (h *SandboxHandlers) execStreamTTY(c *gin.Context, id, command string, req gen.ExecStreamRequest, flusher http.Flusher) {
	cmd, cleanup, err := h.iso.ExecCmd(c.Request.Context(), isolation.ExecConfig{
		Command: command, Cwd: req.Cwd, Env: req.Env, TTY: true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	master, err := pty.Start(cmd)
	if err != nil {
		cleanup()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	sess := pty.NewSession(master, nil)
	h.processes.Store(id, sess)
	defer func() {
		h.processes.Delete(id)
		sess.Close()
		// cleanup reaps the in-workload process tree on client abort.
		cleanup()
	}()

	replay, live, detached, detach, ok := sess.Attach()
	w := c.Writer
	writeSSEHeader(w, flusher)
	ranToEnd := true
	if ok {
		defer detach()
		ranToEnd = streamSession(w, flusher, replay, live, detached, sess.Done(), c.Request.Context().Done())
	}

	exitCode := waitExitCode(cmd)
	if ranToEnd {
		writeExitFrame(w, flusher, exitCode)
	}
}

// execStreamAttach streams the sandbox entrypoint's TTY to the client and
// routes the client's stdin/resizes back to it. Multiple clients may attach
// concurrently; each gets the recent scrollback followed by live output. The
// stream stays open until the client disconnects or the entrypoint exits.
func (h *SandboxHandlers) execStreamAttach(c *gin.Context, id string, flusher http.Flusher) {
	sess := h.entrypointTTY
	if sess == nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "no entrypoint tty: sandbox is not configured with tty: true"})
		return
	}
	replay, live, detached, detach, ok := sess.Attach()
	w := c.Writer
	writeSSEHeader(w, flusher)
	if !ok {
		// The entrypoint already exited; report a clean terminal close.
		writeExitFrame(w, flusher, 0)
		return
	}
	defer detach()
	h.processes.Store(id, sess)
	defer h.processes.Delete(id)

	if streamSession(w, flusher, replay, live, detached, sess.Done(), c.Request.Context().Done()) {
		writeExitFrame(w, flusher, 0)
	}
}

// streamSession replays buffered output then forwards live output from a
// tty.Session to the SSE writer. It returns true if the session ended (the
// caller should write an exit frame) or false if the client disconnected
// first (connection is gone — nothing more to write).
func streamSession(w gin.ResponseWriter, flusher http.Flusher, replay [][]byte, live <-chan []byte, detached, sessDone, clientGone <-chan struct{}) bool {
	emit := func(text string) {
		payload, _ := json.Marshal(map[string]string{"type": "stdout", "text": text})
		fmt.Fprintf(w, "event: stdout\ndata: %s\n\n", payload)
		flusher.Flush()
	}
	for _, chunk := range replay {
		emit(string(chunk))
	}
	for {
		select {
		case chunk := <-live:
			emit(string(chunk))
		case <-detached:
			return true
		case <-sessDone:
			// Drain any output buffered right before the process exited.
			for {
				select {
				case chunk := <-live:
					emit(string(chunk))
				default:
					return true
				}
			}
		case <-clientGone:
			return false
		}
	}
}

func (h *SandboxHandlers) execStreamPipes(c *gin.Context, id string, req gen.ExecStreamRequest, flusher http.Flusher) {
	command := derefString(req.Command)
	reqEventID := h.publishExecRequest(gen.ExecRequest{Command: command, Cwd: req.Cwd})
	w := c.Writer

	cmd, cleanup, err := h.iso.ExecCmd(c.Request.Context(), isolation.ExecConfig{
		Command: command, Cwd: req.Cwd, Env: req.Env,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cleanup()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	h.processes.Store(id, stdinPipe)
	defer func() {
		h.processes.Delete(id)
		stdinPipe.Close()
		// cleanup reaps the in-workload process tree on client abort.
		cleanup()
	}()

	writeSSEHeader(w, flusher)

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

	exitCode := waitExitCode(cmd)

	h.publishExecResponse(reqEventID)

	writeExitFrame(w, flusher, exitCode)
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

	if p, ok := val.(ttyStdin); ok {
		// CSI 8 ; rows ; cols t — XTWINOPS resize sequence sent by the client on
		// terminal resize. Intercept it to set the PTY window size instead of
		// forwarding it to the shell.
		var rows, cols uint16
		if n, _ := fmt.Sscanf(body.Data, "\x1b[8;%d;%dt", &rows, &cols); n == 2 {
			_ = p.Resize(rows, cols)
			c.Status(http.StatusNoContent)
			return
		}
		if _, err := io.WriteString(p, body.Data); err != nil {
			c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
			return
		}
	} else if w, ok := val.(io.Writer); ok {
		if _, err := io.WriteString(w, body.Data); err != nil {
			c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
			return
		}
	}
	c.Status(http.StatusNoContent)
}

// writeSSEHeader writes the SSE response headers and flushes them so the
// client sees the stream open before any frame arrives.
func writeSSEHeader(w gin.ResponseWriter, flusher http.Flusher) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
}

// writeExitFrame writes the terminal "exit" SSE frame carrying the process
// exit code and flushes it; the stream closes after this frame.
func writeExitFrame(w gin.ResponseWriter, flusher http.Flusher, code int) {
	payload, _ := json.Marshal(map[string]any{"type": "exit", "code": code})
	fmt.Fprintf(w, "event: exit\ndata: %s\n\n", payload)
	flusher.Flush()
}

// waitExitCode reaps cmd and returns its exit code, treating a non-zero exit
// as a code rather than a Go error.
func waitExitCode(cmd *exec.Cmd) int {
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
	}
	return 0
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
