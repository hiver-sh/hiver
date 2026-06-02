package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/events"
	mcpapi "github.com/blasten/hive/internal/mcp"
	"github.com/blasten/hive/internal/spec"
)

// agentContainerID is the runc container ID sandboxd always assigns the
// agent. sandboxd runs as PID 1 in the sandbox pod, so os.Getpid() == 1.
const agentContainerID = "agent-1"

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
	broker     *events.Broker
	store      configStore
	lifetime   lifetime
	upperDir   string       // host-side path of the overlayfs upper layer
	processes  sync.Map     // id → io.Writer (stdin of a running exec-stream process; pty master for tty sessions)
	mcpHandler http.Handler // MCP Streamable HTTP handler backed by the runc container
}

func NewSandboxHandlers(broker *events.Broker, store configStore, lifetime lifetime, upperDir string) *SandboxHandlers {
	h := &SandboxHandlers{
		broker:   broker,
		store:    store,
		lifetime: lifetime,
		upperDir: upperDir,
	}
	h.mcpHandler = mcpapi.NewContainerHandler(h.execCommand, h.resolveAgentPath)
	return h
}

// execCommand is the core of the Exec handler: runs command inside the agent
// container via runc exec and returns buffered stdout, stderr, and exit code.
// A non-zero exit code is not treated as a Go error.
func (h *SandboxHandlers) execCommand(ctx context.Context, command string, cwd *string, env *map[string]string) (stdout, stderr string, exitCode int, err error) {
	if err = waitForContainer(ctx, agentContainerID); err != nil {
		return
	}

	pidPath, err := newExecPIDFile()
	if err != nil {
		return
	}
	defer func() {
		if ctx.Err() != nil {
			killExecTree(pidPath)
		}
		os.Remove(pidPath)
	}()

	cmd := exec.CommandContext(ctx, "runc", buildRuncExecArgs(command, cwd, false, env, pidPath)...)
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

// resolveAgentPath maps an agent-visible absolute path to its host-side path
// by loading the current config and delegating to resolveHostPath.
func (h *SandboxHandlers) resolveAgentPath(agentPath string) (string, error) {
	cfg, err := h.store.Get()
	if err != nil {
		return "", err
	}
	return h.resolveHostPath(cfg, filepath.Clean(agentPath)), nil
}

// resolveHostPath maps an agent-visible absolute path to its host-side path.
// FUSE mount backends take priority (longest-prefix match on cfg.Fs); all
// other paths fall back to the overlayfs upper layer so the caller can read
// or write any file the container has touched.
func (h *SandboxHandlers) resolveHostPath(cfg gen.SandboxConfig, cleaned string) string {
	var matchedMount string
	for _, fs := range cfg.Fs {
		m := fsBase(fs).Mount
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
		}
	}
	return cfg
}
