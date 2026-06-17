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

// TestExecE2E exercises POST /v1/exec and POST /v1/exec-stream against a
// hiversh/python:3.13-alpine sandbox. The suite covers the buffered Exec
// path (stdout, stderr, non-zero exit, CWD, env injection) and the
// streaming ExecStream path (chunked output and stdin delivery).
func TestExecE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	key := fmt.Sprintf("e2e-exec-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "hiversh/python:3.13-alpine",
		Entrypoint: []string{"tail", "-f", "/dev/null"},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))
	t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}

	t.Run("exec_stdout", func(t *testing.T) {
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: "echo hello-exec"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("exit_code=%d, want 0", res.ExitCode)
		}
		if !strings.Contains(res.Stdout, "hello-exec") {
			t.Errorf("stdout=%q, want 'hello-exec'", res.Stdout)
		}
	})

	t.Run("exec_nonzero_exit", func(t *testing.T) {
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: "exit 42"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 42 {
			t.Errorf("exit_code=%d, want 42", res.ExitCode)
		}
	})

	t.Run("exec_cwd", func(t *testing.T) {
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: "pwd", CWD: "/tmp"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("exit_code=%d, want 0", res.ExitCode)
		}
		if !strings.Contains(res.Stdout, "/tmp") {
			t.Errorf("stdout=%q, want '/tmp'", res.Stdout)
		}
	})

	t.Run("exec_env", func(t *testing.T) {
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
			Command: "echo $HIVE_TEST_VAR",
			Env:     map[string]string{"HIVE_TEST_VAR": "injected-value"},
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(res.Stdout, "injected-value") {
			t.Errorf("stdout=%q, want 'injected-value'", res.Stdout)
		}
	})

	t.Run("exec_argv_array", func(t *testing.T) {
		// An array command is executed as literal argv (no shell), so the
		// "$HOME" token is printed verbatim rather than expanded.
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
			Command: []string{"echo", "$HOME not-expanded"},
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("exit_code=%d, want 0", res.ExitCode)
		}
		if !strings.Contains(res.Stdout, "$HOME not-expanded") {
			t.Errorf("stdout=%q, want literal '$HOME not-expanded'", res.Stdout)
		}
	})

	t.Run("exec_stream_argv_array", func(t *testing.T) {
		proc, err := sbx.ExecStream(ctx, hiverclient.ExecStreamRequest{
			Command: []string{"echo", "argv stream"},
		})
		if err != nil {
			t.Fatalf("ExecStream: %v", err)
		}
		var got strings.Builder
		for frame := range proc.Output {
			got.WriteString(frame.Stdout)
			got.WriteString(frame.Stderr)
		}
		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if code != 0 {
			t.Errorf("exit_code=%d, want 0", code)
		}
		if !strings.Contains(got.String(), "argv stream") {
			t.Errorf("output=%q, want 'argv stream'", got.String())
		}
	})

	t.Run("exec_stderr", func(t *testing.T) {
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
			Command: `python3 -c "import sys; sys.stderr.write('err-line\n')"`,
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(res.Stderr, "err-line") {
			t.Errorf("stderr=%q, want 'err-line'", res.Stderr)
		}
	})

	t.Run("exec_stream_stdout", func(t *testing.T) {
		proc, err := sbx.ExecStream(ctx, hiverclient.ExecStreamRequest{
			Command: "echo streaming-line",
		})
		if err != nil {
			t.Fatalf("ExecStream: %v", err)
		}
		var got strings.Builder
		for frame := range proc.Output {
			got.WriteString(frame.Stdout)
			got.WriteString(frame.Stderr)
		}
		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if code != 0 {
			t.Errorf("exit_code=%d, want 0", code)
		}
		if !strings.Contains(got.String(), "streaming-line") {
			t.Errorf("output=%q, want 'streaming-line'", got.String())
		}
	})

	t.Run("exec_stream_tty", func(t *testing.T) {
		// With TTY:true the process should run inside a PTY; tty(1) exits 0
		// and prints the device path only when stdin is a real terminal.
		proc, err := sbx.ExecStream(ctx, hiverclient.ExecStreamRequest{
			Command: "tty",
			TTY:     true,
		})
		if err != nil {
			t.Fatalf("ExecStream: %v", err)
		}
		var got strings.Builder
		for frame := range proc.Output {
			got.WriteString(frame.Stdout)
			got.WriteString(frame.Stderr)
		}
		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if code != 0 {
			t.Errorf("exit_code=%d, want 0 (stdin must be a tty)", code)
		}
		if !strings.Contains(got.String(), "/dev/") {
			t.Errorf("output=%q, want a /dev/* path from tty(1)", got.String())
		}
	})

	t.Run("exec_stream_stdin", func(t *testing.T) {
		// python3 reads exactly one line then exits, letting us verify
		// that WriteStdin delivers bytes to the running process.
		proc, err := sbx.ExecStream(ctx, hiverclient.ExecStreamRequest{
			Command: `python3 -c "import sys; print('got: ' + sys.stdin.readline().strip())"`,
		})
		if err != nil {
			t.Fatalf("ExecStream: %v", err)
		}
		if err := proc.WriteStdin(ctx, "from-stdin\n"); err != nil {
			t.Fatalf("WriteStdin: %v", err)
		}
		var got strings.Builder
		for frame := range proc.Output {
			got.WriteString(frame.Stdout)
		}
		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if code != 0 {
			t.Errorf("exit_code=%d, want 0", code)
		}
		if !strings.Contains(got.String(), "from-stdin") {
			t.Errorf("stdout=%q, want 'from-stdin'", got.String())
		}
	})
}
