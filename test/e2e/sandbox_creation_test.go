package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestSandboxCreationE2E exercises the entrypoint, cwd, and tty fields of
// SandboxConfig. Each sub-test provisions its own sandbox so the options are
// isolated and independent.
func TestSandboxCreationE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// entrypoint: verify that a custom entrypoint is actually running as
	// PID 1. strings.Fields splits the entrypoint on whitespace, so only
	// argv-safe tokens (no shell quoting) work reliably.
	// "sleep 3600" → ["sleep","3600"] — clear and distinctive.
	t.Run("entrypoint", func(t *testing.T) {
		key := fmt.Sprintf("e2e-create-ep-%d", time.Now().UnixNano())
		config := hiverclient.SandboxConfig{
			Image:      "hiversh/python:3.13-alpine",
			Entrypoint: []string{"sleep", "3600"},
		}
		t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

		sbx, err := c.GetOrCreateSandbox(ctx, key, config)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox: %v", err)
		}

		// /proc/1/cmdline is NUL-separated; tr converts it to spaces.
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
			Command: "cat /proc/1/cmdline | tr '\\0' ' '",
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("exit_code=%d, want 0; stderr=%q", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "sleep") {
			t.Errorf("stdout=%q, want 'sleep' in PID 1 cmdline", res.Stdout)
		}
	})

	// cwd: verify that SandboxConfig.CWD sets the working directory of PID 1.
	// /proc/1/cwd is a symlink to the process's current working directory;
	// readlink resolves it to the absolute path.
	// Use /tmp — it is guaranteed to exist in any Alpine image.
	t.Run("cwd", func(t *testing.T) {
		key := fmt.Sprintf("e2e-create-cwd-%d", time.Now().UnixNano())
		config := hiverclient.SandboxConfig{
			Image:      "hiversh/python:3.13-alpine",
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			CWD:        "/tmp",
		}
		t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

		sbx, err := c.GetOrCreateSandbox(ctx, key, config)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox: %v", err)
		}

		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: "readlink /proc/1/cwd"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("exit_code=%d, want 0; stderr=%q", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "/tmp") {
			t.Errorf("stdout=%q, want '/tmp' from readlink /proc/1/cwd", res.Stdout)
		}
	})

	// env: verify that SandboxConfig.Env vars are present inside the container.
	// printenv exits 0 and prints the value when the variable exists; a missing
	// variable would produce empty output, which the check below rejects.
	t.Run("env", func(t *testing.T) {
		key := fmt.Sprintf("e2e-create-env-%d", time.Now().UnixNano())
		config := hiverclient.SandboxConfig{
			Image:      "hiversh/python:3.13-alpine",
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			Env: map[string]string{
				"MY_VAR":    "hello",
				"OTHER_VAR": "world",
			},
		}
		t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

		sbx, err := c.GetOrCreateSandbox(ctx, key, config)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox: %v", err)
		}

		for varName, want := range config.Env {
			res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
				Command: fmt.Sprintf("printenv %s", varName),
			})
			if err != nil {
				t.Fatalf("Exec printenv %s: %v", varName, err)
			}
			if res.ExitCode != 0 {
				t.Errorf("%s: exit_code=%d, want 0; stderr=%q", varName, res.ExitCode, res.Stderr)
			}
			got := strings.TrimSpace(res.Stdout)
			if got != want {
				t.Errorf("%s: got %q, want %q", varName, got, want)
			}
		}
	})

	// tty: verify that SandboxConfig.TTY:true configures a PTY session for
	// the entrypoint. The observable is that ExecStream with an empty command
	// (which attaches to the entrypoint's TTY session) connects without error.
	// If tty:true is not applied the server returns HTTP 400 "not configured",
	// which surfaces as a non-nil error from ExecStream.
	t.Run("tty", func(t *testing.T) {
		key := fmt.Sprintf("e2e-create-tty-%d", time.Now().UnixNano())
		config := hiverclient.SandboxConfig{
			Image:      "hiversh/python:3.13-alpine",
			Entrypoint: []string{"sleep", "3600"},
			TTY:        true,
		}
		t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

		sbx, err := c.GetOrCreateSandbox(ctx, key, config)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox: %v", err)
		}

		// A 5-second timeout gives the server time to start streaming;
		// we cancel immediately after the connection is established.
		attachCtx, attachCancel := context.WithTimeout(ctx, 5*time.Second)
		defer attachCancel()

		proc, err := sbx.ExecStream(attachCtx, hiverclient.ExecStreamRequest{Command: ""})
		if err != nil {
			t.Fatalf("ExecStream attach: tty:true not applied — %v", err)
		}
		attachCancel()
		for range proc.Output {
		}
		_, _ = proc.Wait()
	})
}
