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

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/events"
	"github.com/blasten/hive/internal/isolation"
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
	processes sync.Map            // id → io.Writer (stdin of a running exec-stream process; pty master for tty sessions)
	netMark   int                 // SO_MARK for the reverse proxy dialer, bypasses iptables REDIRECT
}

func NewSandboxHandlers(broker *events.Broker, store configStore, lifetime lifetime, iso isolation.Isolation, netMark int) *SandboxHandlers {
	return &SandboxHandlers{
		broker:   broker,
		store:    store,
		lifetime: lifetime,
		iso:      iso,
		netMark:  netMark,
	}
}

// execCommand is the core of the Exec handler: runs command inside the agent
// workload via the isolation backend and returns buffered stdout, stderr, and
// exit code. A non-zero exit code is not treated as a Go error.
func (h *SandboxHandlers) execCommand(ctx context.Context, command string, cwd *string, env *map[string]string) (stdout, stderr string, exitCode int, err error) {
	if err = h.iso.WaitReady(ctx); err != nil {
		return
	}

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
		}
	}
	return cfg
}
