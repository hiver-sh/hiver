package handlers

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
	"sync"
	"syscall"
	"time"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/isolation"
	"github.com/creack/pty"
	"github.com/gin-gonic/gin"
	"golang.org/x/term"
)

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
// Each line is also published to the broker as a StdioEvent.
func (h *SandboxHandlers) ExecStream(c *gin.Context, id string) {
	var req gen.ExecStreamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}
	if err := h.iso.WaitReady(c.Request.Context()); err != nil {
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

	cmd, cleanup, err := h.iso.ExecCmd(c.Request.Context(), isolation.ExecConfig{
		Command: req.Command, Cwd: req.Cwd, Env: req.Env, TTY: true,
	})
	if err != nil {
		master.Close()
		slave.Close()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Make the outer pty the runtime's controlling terminal (Setsid makes it
	// a session leader; Setctty adopts fd 0 — the slave — as its controlling
	// tty). Without this the kernel never delivers SIGWINCH when we resize the
	// master, so window-size changes never reach the in-container pty and the
	// program is stuck at the startup default size (stale content survives clears).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		cleanup()
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	slave.Close() // the runtime dup'd it at exec; the parent no longer needs it.

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
		// cleanup reaps the in-workload process tree on client abort.
		cleanup()
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

	cmd, cleanup, err := h.iso.ExecCmd(c.Request.Context(), isolation.ExecConfig{
		Command: req.Command, Cwd: req.Cwd, Env: req.Env,
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
