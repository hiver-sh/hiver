package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiver-sh/hiver/internal/spec"
	"github.com/hiver-sh/hiver/test/e2e/setup"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	moduleRoot  = "../.."
	fixtureName = "mcp-server"
)

// TestMcpServerE2E brings up the mcp-server fixture and drives the
// streamable HTTP MCP endpoint with the Go SDK client, exercising one
// call per registered tool.
func TestMcpServerE2E(t *testing.T) {
	pod, session, ctx, cancel := startMcpFixture(t)
	defer pod.stop()
	defer cancel()
	defer session.Close()

	t.Run("bash_ls_scratch", func(t *testing.T) {
		var out struct {
			Stdout, Stderr string
			ExitCode       int
		}
		callMCP(t, ctx, session, "bash", map[string]any{"cmd": "ls -la /scratch"}, &out)
		if out.ExitCode != 0 {
			t.Fatalf("ls exit=%d, stderr=%q", out.ExitCode, out.Stderr)
		}
		t.Logf("ls -la /scratch:\n%s", out.Stdout)
	})

	t.Run("write_creates_file", func(t *testing.T) {
		var out struct{ Bytes int }
		callMCP(t, ctx, session, "write", map[string]any{
			"path":    "/scratch/mcp-e2e.txt",
			"content": "hello\nworld\n",
		}, &out)
		if out.Bytes != 12 {
			t.Errorf("bytes=%d, want 12", out.Bytes)
		}
	})

	t.Run("read_returns_written_content", func(t *testing.T) {
		var out struct {
			Content   string
			LineCount int
			Truncated bool
		}
		callMCP(t, ctx, session, "read", map[string]any{"path": "/scratch/mcp-e2e.txt"}, &out)
		if !strings.Contains(out.Content, "hello") || !strings.Contains(out.Content, "world") {
			t.Errorf("content = %q, want hello+world", out.Content)
		}
	})

	t.Run("edit_replaces_substring", func(t *testing.T) {
		var out struct{ Replacements int }
		callMCP(t, ctx, session, "edit", map[string]any{
			"path":      "/scratch/mcp-e2e.txt",
			"oldString": "hello",
			"newString": "HELLO",
		}, &out)
		if out.Replacements != 1 {
			t.Errorf("replacements=%d, want 1", out.Replacements)
		}
	})

	t.Run("glob_finds_written_file", func(t *testing.T) {
		var out struct{ Paths []string }
		callMCP(t, ctx, session, "glob", map[string]any{
			"pattern": "*.txt",
			"root":    "/scratch",
		}, &out)
		found := false
		for _, p := range out.Paths {
			if strings.HasSuffix(p, "mcp-e2e.txt") {
				found = true
			}
		}
		if !found {
			t.Errorf("glob did not find mcp-e2e.txt; got %v", out.Paths)
		}
	})

	t.Run("grep_finds_match", func(t *testing.T) {
		var out struct {
			Matches []struct {
				Path string
				Line int
				Text string
			}
		}
		callMCP(t, ctx, session, "grep", map[string]any{
			"pattern": "world",
			"path":    "/scratch",
		}, &out)
		for _, m := range out.Matches {
			if strings.HasSuffix(m.Path, "mcp-e2e.txt") && m.Line == 2 {
				return
			}
		}
		t.Errorf("expected match in mcp-e2e.txt:2, got %+v", out.Matches)
	})
}

// startMcpFixture builds the mcp-server fixture, starts the pod, and
// connects an MCP client session. Skips the test when the fixture's
// gdrive credentials aren't in the env.
func startMcpFixture(t *testing.T) (*mcpPod, *mcp.ClientSession, context.Context, context.CancelFunc) {
	t.Helper()
	setup.RequireDocker(t)

	if setup.GetEnv("HIVE_RUN_GDRIVE_TESTS") == "" {
		t.Skip("set HIVE_RUN_GDRIVE_TESTS=1 (and HIVE_GDRIVE_ACCESS_TOKEN etc.) to run GDrive tests")
	}
	token := setup.GetEnv("HIVE_GDRIVE_ACCESS_TOKEN")
	if token == "" {
		t.Skip("set HIVE_GDRIVE_ACCESS_TOKEN [+ HIVE_GDRIVE_FOLDER_ID] to run")
	}

	fixtureDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures", fixtureName))
	if err != nil {
		t.Fatalf("abs fixture dir: %v", err)
	}
	sp, err := spec.Parse(filepath.Join(fixtureDir, "spec.json"))
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	gd := &sp.FS[0]
	gd.GdriveAccessToken = token
	gd.GdriveRefreshToken = setup.GetEnv("HIVE_GDRIVE_REFRESH_TOKEN")
	gd.GdriveClientID = setup.GetEnv("HIVE_GDRIVE_CLIENT_ID")
	gd.GdriveClientSecret = setup.GetEnv("HIVE_GDRIVE_CLIENT_SECRET")
	gd.GdriveFolderID = setup.GetEnv("HIVE_GDRIVE_FOLDER_ID")
	if err := sp.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	rendered, err := json.Marshal(sp)
	if err != nil {
		t.Fatalf("re-render spec: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(specPath, rendered, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	sandboxDir := sp.Image
	if !filepath.IsAbs(sandboxDir) {
		sandboxDir = filepath.Join(fixtureDir, sandboxDir)
	}
	buildContext := sandboxDir
	if rel, err := filepath.Rel(fixtureDir, sandboxDir); err == nil && strings.HasPrefix(rel, "..") {
		buildContext, err = filepath.Abs(moduleRoot)
		if err != nil {
			t.Fatalf("abs module root: %v", err)
		}
	}
	dockerfile := sandboxDir
	if info, err := os.Stat(sandboxDir); err == nil && info.IsDir() {
		dockerfile = filepath.Join(sandboxDir, "Dockerfile")
	}
	agentImage := "sandbox-" + fixtureName + ":e2e"
	bundleImage := "sandbox-bundle-" + fixtureName + ":e2e"
	setup.BuildImages(t, dockerfile, buildContext, agentImage)
	setup.BuildSandboxBundle(t, agentImage, bundleImage)

	pod := startMCPPod(t, bundleImage, specPath)

	mcpURL := "http://127.0.0.1:8080/v1/mcp"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	session := connectMCP(t, ctx, mcpURL, &pod.out)
	return pod, session, ctx, cancel
}

// mcpPod is a running sandbox-pod whose agent is the MCP server. The
// caller must invoke stop() at the end of the test.
type mcpPod struct {
	stop func()
	out  bytes.Buffer
}

// startMCPPod runs sandbox-runtime with the MCP agent and publishes
// :8080 (sandboxd API, which also serves /v1/mcp) to the host. Unlike
// runSandboxPod it doesn't wait for any agent-side "DONE" marker — the
// readiness check is "MCP server answers initialize", which the caller
// does via connectMCP.
func startMCPPod(t *testing.T, bundleImage, specPath string) *mcpPod {
	t.Helper()

	containerName := fmt.Sprintf("sandbox-pod-mcp-e2e-%d", time.Now().UnixNano())
	args := []string{
		"run", "--rm", "--name", containerName,
		"--device", "/dev/fuse",
		"--cap-add", "SYS_ADMIN", "--cap-add", "NET_ADMIN", "--cap-add", "MKNOD",
		"--cap-add", "SYS_CHROOT", "--cap-add", "SETPCAP", "--cap-add", "SETFCAP",
		"--cap-add", "SETUID", "--cap-add", "SETGID",
		"--cap-add", "DAC_READ_SEARCH", "--cap-add", "FOWNER", "--cap-add", "CHOWN",
		"--security-opt", "apparmor=unconfined",
		"--security-opt", "seccomp=unconfined",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"-p", "8080:8080",
		"-v", specPath + ":/mnt/spec.json:ro",
		bundleImage,
		"--spec", "/mnt/spec.json",
	}

	pod := &mcpPod{}
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &pod.out, &pod.out
	if err := cmd.Start(); err != nil {
		t.Fatalf("docker run start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	pod.stop = func() {
		_ = exec.Command("docker", "kill", "-s", "TERM", containerName).Run()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = exec.Command("docker", "kill", containerName).Run()
			<-done
		}
		if t.Failed() {
			t.Logf("pod output:\n%s", pod.out.String())
		}
	}
	return pod
}

func connectMCP(t *testing.T, ctx context.Context, url string, podOut *bytes.Buffer) *mcp.ClientSession {
	return setup.ConnectMCP(t, ctx, url, podOut)
}

func callMCP(t *testing.T, ctx context.Context, sess *mcp.ClientSession, name string, args map[string]any, out any) {
	setup.CallMCP(t, ctx, sess, name, args, out)
}
