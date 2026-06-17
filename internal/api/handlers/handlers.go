package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/pty"
)

// ErrApplyInProgress reports that a previous ApplyConfig call is still
// running; the handler translates this to HTTP 409.
var ErrApplyInProgress = errors.New("a previous apply is still in progress")

type configStore interface {
	Get() (gen.SandboxConfig, error)
	Apply(gen.SandboxConfig) (gen.Changes, error)
}

type lifetime interface {
	Reset()
}

type SandboxHandlers struct {
	broker    *events.Broker
	store     configStore
	lifetime  lifetime
	iso       isolation.Isolation // runtime boundary: exec + filesystem access
	processes sync.Map            // id → io.Writer (stdin of a running exec-stream process) or *pty.Session
	netMark   int                 // SO_MARK for the reverse proxy dialer, bypasses iptables REDIRECT

	// readyCh closes when the inner sandbox is up and running (NotifyReady). The
	// server starts serving the instant the process starts — before the workload,
	// and before its own subsystems, exist — so it refuses requests until then
	// (the /v1/ping probe excepted; see Ready). Observing the close also publishes
	// the injected subsystem fields above: each is set before NotifyReady, so a
	// reader past the gate sees them without further synchronization.
	readyOnce sync.Once
	readyCh   chan struct{}

	// entrypointTTY is the pty wrapping the sandbox entrypoint when the config
	// sets tty: true; nil otherwise. The entrypoint launches after the API
	// server is already serving, so it's published later via SetEntrypointTTY
	// and read atomically — attaches see nil until then. An exec-stream request
	// with an empty command attaches to it (see execStreamAttach).
	entrypointTTY atomic.Pointer[pty.Session]

	// started flips true when the workload is launched (the agent process is
	// committed with its boot-time config). Until then the sandbox is "prewarm":
	// ApplyConfig may still set the boot-time-only fields (cpu, memory,
	// entrypoint, cwd, tty, env), which is how a --prewarm sandbox is configured
	// by its first apply. Afterward those fields are frozen (freezeStartedFields).
	started atomic.Bool
}

// NewSandboxHandlers builds the handlers with only netMark, which is a fixed
// constant known up front; the remaining subsystems are injected via the SetX
// methods as boot creates them.
func NewSandboxHandlers(netMark int) *SandboxHandlers {
	return &SandboxHandlers{netMark: netMark, readyCh: make(chan struct{})}
}

// The setters below inject sandboxd's subsystems as boot creates them, while the
// server is already serving. Each is called exactly once, from the single
// startup goroutine, and all complete before NotifyReady, which publishes them.
func (h *SandboxHandlers) SetBroker(b *events.Broker)           { h.broker = b }
func (h *SandboxHandlers) SetStore(s configStore)               { h.store = s }
func (h *SandboxHandlers) SetLifetime(l lifetime)               { h.lifetime = l }
func (h *SandboxHandlers) SetIsolation(iso isolation.Isolation) { h.iso = iso }

// NotifyReady signals that the inner sandbox is up and running, flipping the
// server from refusing requests (500, or 503 on /v1/ping) to serving them.
// Idempotent; called once from the startup goroutine when readiness is observed.
func (h *SandboxHandlers) NotifyReady() {
	h.readyOnce.Do(func() { close(h.readyCh) })
}

// Ready reports whether NotifyReady has fired — i.e. the sandbox can serve real
// requests. The server answers 500/503 until this returns true.
func (h *SandboxHandlers) Ready() bool {
	select {
	case <-h.readyCh:
		return true
	default:
		return false
	}
}

// SetStarted marks the workload as launched, freezing the boot-time-only config
// fields against further ApplyConfig changes. Called once, when the agent is
// started (immediately at boot in the normal flow; at first config-apply in the
// prewarm flow). Idempotent.
func (h *SandboxHandlers) SetStarted() { h.started.Store(true) }

// Started reports whether the workload has been launched. ApplyConfig uses it to
// decide whether the boot-time-only fields may still be set (prewarm) or must be
// frozen (started).
func (h *SandboxHandlers) Started() bool { return h.started.Load() }

// WaitReady blocks until NotifyReady fires or ctx is done, returning ctx.Err()
// if it gives up first. Backs the /v1/ping?block=true long-poll.
func (h *SandboxHandlers) WaitReady(ctx context.Context) error {
	select {
	case <-h.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ResetLifetime restarts the inactivity countdown. Call only once the sandbox
// is ready: lifetime is wired (and safely published) by then, so callers must
// gate on Ready first — before that it is nil and racing with injection.
func (h *SandboxHandlers) ResetLifetime() {
	// nil in prewarm mode, where the API serves (so a config can be applied)
	// before any lifetime/TTL has been wired.
	if h.lifetime != nil {
		h.lifetime.Reset()
	}
}

// SetEntrypointTTY publishes the entrypoint's pty session so exec-stream attach
// requests can reach it. Called once, after the entrypoint launches (the API
// server is already serving by then), so reads of it are atomic.
func (h *SandboxHandlers) SetEntrypointTTY(sess *pty.Session) {
	h.entrypointTTY.Store(sess)
}

// execCommand is the core of the Exec handler: runs command inside the agent
// workload via the isolation backend and returns buffered stdout, stderr, and
// exit code. A non-zero exit code is not treated as a Go error.
func (h *SandboxHandlers) execCommand(ctx context.Context, command string, cwd *string, env *map[string]string) (stdout, stderr string, exitCode int, err error) {
	// Readiness is guaranteed by /v1/ping, which blocks until the workload is
	// running; clients ping before they exec, so the workload is up by here.
	cmd, cleanup, err := h.iso.ExecCmd(ctx, isolation.ExecConfig{Command: command, Cwd: cwd, Env: env})
	if err != nil {
		return
	}
	defer cleanup()

	stdoutPipe, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		err = pipeErr
		return
	}
	stderrPipe, pipeErr := cmd.StderrPipe()
	if pipeErr != nil {
		err = pipeErr
		return
	}
	if err = cmd.Start(); err != nil {
		return
	}

	var wg sync.WaitGroup
	var stdoutBuf, stderrBuf strings.Builder
	var stdoutMu, stderrMu sync.Mutex
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

	if waitErr := cmd.Wait(); waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			err = waitErr
			return
		}
		exitCode = exitErr.ExitCode()
	}
	stdout = stdoutBuf.String()
	stderr = stderrBuf.String()
	return
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
		case gen.BackendExternal:
			if v, err := fs.AsExternalFileSystem(); err == nil {
				v.Acls = acls
				_ = cfg.Fs[i].FromExternalFileSystem(v)
			}
		}
	}
	return cfg
}
