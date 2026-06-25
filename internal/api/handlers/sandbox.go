package handlers

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/pty"
)

// Sandbox is one sandbox within the pod: the per-key state and runtime
// subsystems that used to live as process-wide singletons on SandboxHandlers.
// The supervisor (cmd/sandboxd) owns the map of these, wires each one's
// subsystems as boot creates them, and drives its lifecycle; the keyed API
// handlers resolve a Sandbox by key and operate on it.
type Sandbox struct {
	key string

	broker   *events.Broker
	store    configStore
	lifetime lifetime
	iso      isolation.Isolation // runtime boundary: exec + filesystem access
	netMark  int                 // SO_MARK for the reverse proxy dialer, bypasses iptables REDIRECT

	// lifecycleCtx is the sandbox's lifecycle context; it's cancelled when the
	// sandbox is torn down (DELETE, agent exit, or pod shutdown). Exec sessions
	// tie their host-side bridge process to it (via execContext) so a delete
	// kills in-flight execs instead of leaving them blocked — e.g. a microvm
	// exec's sbxvsock otherwise stalls forever on a TCP read to the now-dead
	// guest, which sends no RST when its VMM is killed. Set once, before
	// NotifyReady; nil for sandboxes that never wired it (exec falls back to the
	// request context alone).
	lifecycleCtx context.Context

	// proxyHost is the host the ingress reverse proxy dials to reach the user's
	// workload. The boot sandbox shares the pod netns, so its workload is on
	// 127.0.0.1 (the default). A packed sandbox runs in its own netns reachable
	// only at its guest IP (172.16.<n>.2), set via SetProxyHost.
	proxyHost string

	// proxyTransport is the shared, keep-alive HTTP transport the ingress reverse
	// proxy reuses across requests. netMark and proxyHost are fixed after
	// readiness, so one transport per sandbox is correct — it pools connections to
	// the workload instead of dialing (and allocating a Transport) per request.
	// Built lazily by proxyRoundTripper. Nil when no SO_MARK is set (the proxy
	// then falls back to http.DefaultTransport, which is itself pooled).
	proxyTransportOnce sync.Once
	proxyTransport     *http.Transport

	// processes maps an exec-stream id → io.Writer (the running process's stdin)
	// or *pty.Session. Per sandbox so ids can't collide across keys.
	processes sync.Map

	// readyCh closes when this sandbox's workload is up (NotifyReady). The
	// subsystem fields above are all set before NotifyReady, so a reader past
	// the gate sees them without further synchronization.
	readyOnce sync.Once
	readyCh   chan struct{}

	// entrypointTTY is the pty wrapping the entrypoint when the config sets
	// tty: true; nil otherwise. Published later (after the entrypoint launches)
	// via SetEntrypointTTY and read atomically.
	entrypointTTY atomic.Pointer[pty.Session]

	// started flips true when the workload is launched (the agent process is
	// committed with its boot-time config). Until then ApplyConfig may still set
	// the boot-time-only fields (cpu, memory, entrypoint, cwd, tty, env).
	started atomic.Bool

	// stopping flips true once teardown has begun (DELETE, the agent exiting, or
	// pod shutdown), so the listing reports "stopping" rather than a stale
	// "running"/"starting" while the workload is being torn down.
	stopping atomic.Bool
}

// NewSandbox builds a sandbox for key with only netMark, a fixed constant known
// up front; the remaining subsystems are injected via the SetX methods as boot
// creates them.
func NewSandbox(key string, netMark int) *Sandbox {
	return &Sandbox{key: key, netMark: netMark, proxyHost: "127.0.0.1", readyCh: make(chan struct{})}
}

// Key reports the sandbox's key.
func (s *Sandbox) Key() string { return s.key }

// SetProxyHost points the ingress reverse proxy at host instead of the default
// 127.0.0.1 — used for packed sandboxes, whose workload lives in a separate
// netns reachable only at the sandbox's guest IP. Called once, before
// NotifyReady.
func (s *Sandbox) SetProxyHost(host string) {
	if host != "" {
		s.proxyHost = host
	}
}

// The setters below inject the sandbox's subsystems as boot creates them, while
// the server is already serving. Each is called once, before NotifyReady, which
// publishes them.
func (s *Sandbox) SetBroker(b *events.Broker)           { s.broker = b }
func (s *Sandbox) SetStore(st configStore)              { s.store = st }
func (s *Sandbox) SetLifetime(l lifetime)               { s.lifetime = l }
func (s *Sandbox) SetIsolation(iso isolation.Isolation) { s.iso = iso }

// SetLifecycleContext wires the sandbox's lifecycle context so exec sessions
// can be cancelled when the sandbox is torn down. Called once, before
// NotifyReady.
func (s *Sandbox) SetLifecycleContext(ctx context.Context) { s.lifecycleCtx = ctx }

// execContext derives a context for an exec session that is cancelled when
// either the request ends or the sandbox is torn down. Without the sandbox tie,
// a DELETE leaves in-flight execs running: the microvm bridge (sbxvsock) blocks
// indefinitely on a read to the killed guest, which never sends a RST. The
// returned cancel must be called to release the AfterFunc registration.
func (s *Sandbox) execContext(reqCtx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(reqCtx)
	if s.lifecycleCtx == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(s.lifecycleCtx, cancel)
	return ctx, func() { stop(); cancel() }
}

// NotifyReady signals that the sandbox's workload is up and running, flipping it
// from refusing requests to serving them. Idempotent.
func (s *Sandbox) NotifyReady() {
	s.readyOnce.Do(func() { close(s.readyCh) })
}

// Ready reports whether NotifyReady has fired — i.e. the sandbox can serve real
// requests.
func (s *Sandbox) Ready() bool {
	select {
	case <-s.readyCh:
		return true
	default:
		return false
	}
}

// WaitReady blocks until NotifyReady fires or ctx is done, returning ctx.Err()
// if it gives up first.
func (s *Sandbox) WaitReady(ctx context.Context) error {
	select {
	case <-s.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetStarted marks the workload as launched, freezing the boot-time-only config
// fields against further ApplyConfig changes. Idempotent.
func (s *Sandbox) SetStarted() { s.started.Store(true) }

// Started reports whether the workload has been launched.
func (s *Sandbox) Started() bool { return s.started.Load() }

// SetStopping marks the sandbox as tearing down. Idempotent.
func (s *Sandbox) SetStopping() { s.stopping.Store(true) }

// Status reports the sandbox's lifecycle phase for the pod listing: stopping
// once teardown has begun, running once the workload is serving, otherwise
// starting.
func (s *Sandbox) Status() gen.SandboxStatus {
	switch {
	case s.stopping.Load():
		return gen.SandboxStatusStopping
	case s.Ready():
		return gen.SandboxStatusRunning
	default:
		return gen.SandboxStatusStarting
	}
}

// ResetLifetime restarts the inactivity countdown. Tolerates a not-yet-wired
// lifetime (prewarm), where the API serves before any TTL has been set up.
func (s *Sandbox) ResetLifetime() {
	if s.lifetime != nil {
		s.lifetime.Reset()
	}
}

// SetEntrypointTTY publishes the entrypoint's pty session so exec-stream attach
// requests can reach it. Called once, after the entrypoint launches.
func (s *Sandbox) SetEntrypointTTY(sess *pty.Session) {
	s.entrypointTTY.Store(sess)
}

// execCommand is the core of the Exec handler: runs command inside the agent
// workload via the isolation backend and returns buffered stdout, stderr, and
// exit code. A non-zero exit code is not treated as a Go error.
func (h *Sandbox) execCommand(ctx context.Context, command string, cwd *string, env *map[string]string) (stdout, stderr string, exitCode int, err error) {
	// Readiness is guaranteed by /v1/ping, which blocks until the workload is
	// running; clients ping before they exec, so the workload is up by here.
	// Tie the exec to the sandbox lifecycle so a DELETE kills it mid-flight.
	ctx, cancelCtx := h.execContext(ctx)
	defer cancelCtx()
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
