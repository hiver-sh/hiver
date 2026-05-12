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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sandbox-platform/agent-sandbox/internal/spec"
	"github.com/sandbox-platform/agent-sandbox/test/e2e/setup"
	"sigs.k8s.io/yaml"
)

// TestMcpServerE2E brings up the mcp-server fixture and drives the
// streamable HTTP MCP endpoint with the Go SDK client, exercising one
// call per registered tool.
func TestMcpServerE2E(t *testing.T) {
	requireDocker(t)

	token := setup.GetEnv("HIVE_GDRIVE_ACCESS_TOKEN")
	if token == "" {
		t.Skip("set HIVE_GDRIVE_ACCESS_TOKEN [+ HIVE_GDRIVE_FOLDER_ID] to run")
	}

	const fixtureName = "mcp-server"
	fixtureDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures", fixtureName))
	if err != nil {
		t.Fatalf("abs fixture dir: %v", err)
	}
	sp, err := spec.Parse(filepath.Join(fixtureDir, "spec.yaml"))
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
	rendered, err := yaml.Marshal(sp)
	if err != nil {
		t.Fatalf("re-render spec: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(specPath, rendered, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	agentDir := sp.Agent.Image
	if !filepath.IsAbs(agentDir) {
		agentDir = filepath.Join(fixtureDir, agentDir)
	}
	buildContext := agentDir
	if rel, err := filepath.Rel(fixtureDir, agentDir); err == nil && strings.HasPrefix(rel, "..") {
		buildContext, err = filepath.Abs(moduleRoot)
		if err != nil {
			t.Fatalf("abs module root: %v", err)
		}
	}
	agentImage := "sandbox-" + fixtureName + ":e2e"
	buildImages(t, agentDir, buildContext, agentImage)
	agentTar := saveAgentImage(t, agentImage)

	auditDir := t.TempDir()
	pod := startMCPPod(t, agentTar, specPath, auditDir)
	defer pod.stop()

	mcpURL := "http://127.0.0.1:8081/v1/mcp"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session := connectMCP(t, ctx, mcpURL, &pod.out)
	defer session.Close()

	t.Run("bash_ls_workspace", func(t *testing.T) {
		var out struct {
			Stdout, Stderr string
			ExitCode       int
		}
		callMCP(t, ctx, session, "bash", map[string]any{"cmd": "ls -la /workspace"}, &out)
		if out.ExitCode != 0 {
			t.Fatalf("ls exit=%d, stderr=%q", out.ExitCode, out.Stderr)
		}
		t.Logf("ls -la /workspace:\n%s", out.Stdout)
	})

	t.Run("write_creates_file", func(t *testing.T) {
		var out struct{ Bytes int }
		callMCP(t, ctx, session, "write", map[string]any{
			"path":    "/workspace/mcp-e2e.txt",
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
		callMCP(t, ctx, session, "read", map[string]any{"path": "/workspace/mcp-e2e.txt"}, &out)
		if !strings.Contains(out.Content, "hello") || !strings.Contains(out.Content, "world") {
			t.Errorf("content = %q, want hello+world", out.Content)
		}
	})

	t.Run("edit_replaces_substring", func(t *testing.T) {
		var out struct{ Replacements int }
		callMCP(t, ctx, session, "edit", map[string]any{
			"path":      "/workspace/mcp-e2e.txt",
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
			"root":    "/workspace",
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
			"path":    "/workspace",
		}, &out)
		for _, m := range out.Matches {
			if strings.HasSuffix(m.Path, "mcp-e2e.txt") && m.Line == 2 {
				return
			}
		}
		t.Errorf("expected match in mcp-e2e.txt:2, got %+v", out.Matches)
	})
}

// mcpPod is a running sandbox-pod whose agent is the MCP server. The
// caller must invoke stop() at the end of the test.
type mcpPod struct {
	stop func()
	out  bytes.Buffer
}

// startMCPPod runs sandbox-runtime with the MCP agent and publishes
// 8081 to the host. Unlike runSandboxPod it doesn't wait for any
// agent-side "DONE" marker — the readiness check is "MCP server
// answers initialize", which the caller does via connectMCP.
func startMCPPod(t *testing.T, agentTar, specPath, auditDir string) *mcpPod {
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
		"-p", "8081:8081",
		"-v", auditDir + ":/audit-out",
		"-v", agentTar + ":/mnt/agent.tar:ro",
		"-v", specPath + ":/mnt/spec.yaml:ro",
		sandboxRuntimeImage,
		"--spec", "/mnt/spec.yaml",
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
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0.0.0"}, nil)
	var lastErr error
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		session, err := client.Connect(dialCtx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
		cancel()
		if err == nil {
			return session
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("MCP server not reachable at %s: %v\npod output:\n%s", url, lastErr, podOut.String())
	return nil
}

func callMCP(t *testing.T, ctx context.Context, sess *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tool %s returned error: %s", name, contentText(res.Content))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal %s structured: %v", name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal %s structured into %T: %v\nraw: %s", name, out, err, raw)
	}
}

func contentText(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
