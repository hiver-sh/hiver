package setup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	moduleRoot          = "../.."
	sandboxRuntimeImage = "hiversh/core:latest"

	// GatewayURL is the Envoy gateway address for the hiver stack
	// started by `hiver up`. Tests that use the controller+gateway
	// stack should call RequireStack(t) and address sandboxes via this URL.
	GatewayURL = "http://localhost:10000"
)

// SummarizeEvents renders the collected stream as one line per event.
// Suitable for `t.Log` / failure logs — full JSON would be unreadable
// for runs with hundreds of events.
func SummarizeEvents(events []map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		id, _ := e["id"].(float64)
		typ, _ := e["type"].(string)
		fmt.Fprintf(&b, "  #%d %s", int(id), typ)
		for _, k := range []string{"access", "host", "path", "method", "operation", "status", "duration_ms", "request_id", "backend"} {
			if v, ok := e[k]; ok {
				fmt.Fprintf(&b, " %s=%v", k, v)
			}
		}
		// stdio carries one of stdout/stderr per event; quote the chunk
		// so trailing whitespace is visible and a runaway newline can't
		// break the one-line-per-event format.
		for _, k := range []string{"stdout", "stderr"} {
			if v, ok := e[k].(string); ok {
				fmt.Fprintf(&b, " %s=%q", k, v)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// redactSecrets masks token-like values in YAML so the rendered-spec
// debug log doesn't leak credentials. We only need the structure +
// presence/absence of fields when debugging the gdrive path.
var secretFieldRE = regexp.MustCompile(`(?m)^(\s*(?:gdrive_access_token|gdrive_refresh_token|gdrive_client_secret|gdrive_service_account_json)\s*:\s*).+$`)

// RequireDocker skips the test if no Docker daemon is reachable.
func RequireDocker(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Skipf("Docker not available: %v", err)
	}
}

// RequireHiverCLI skips the test if the `hiver` CLI is not on PATH. Build
// and link it locally with `make link-cli`.
func RequireHiverCLI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("hiver"); err != nil {
		t.Skipf("hiver CLI not on PATH (run 'make link-cli'): %v", err)
	}
}

// RequireStack skips the test if the hive gateway is not reachable on
// port 10000. Call this at the start of tests that assume `hiver up`
// has already been run.
func RequireStack(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "localhost:10000", 2*time.Second)
	if err != nil {
		t.Skipf("hive gateway not reachable on :10000 (run 'hiver up' first): %v", err)
		return
	}
	conn.Close()
}

// BuildImages builds the two independent images this test needs:
// sandbox-runtime (the pod, always at the module root) and the agent
// image. dockerfile is the absolute path to the agent image's
// Dockerfile; buildContext is the docker build context — usually the
// Dockerfile's directory, but moduleRoot for fixtures that reuse a
// Dockerfile from elsewhere in the repo (see runFixtureE2E for the rule).
func BuildImages(t *testing.T, dockerfile, buildContext, agentImage string) {
	t.Helper()
	build := func(tag, contextDir string, extraArgs ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		args := append([]string{"build", "-t", tag}, extraArgs...)
		args = append(args, contextDir)
		cmd := exec.CommandContext(ctx, "docker", args...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker build %s: %v\n%s", tag, err, out.String())
		}
	}
	build(sandboxRuntimeImage, moduleRoot, "-f", filepath.Join(moduleRoot, "docker/core.Dockerfile"))
	build(agentImage, buildContext, "-f", dockerfile)
}

// BuildSandboxBundle packages agentImage into a sandbox-bundle image tagged
// bundleTag. It saves the agent image as a tarball then builds bundler.Dockerfile
// against it, producing a core image with the agent rootfs pre-extracted at /mnt.
func BuildSandboxBundle(t *testing.T, agentImage, bundleTag string) {
	t.Helper()
	absRoot, err := filepath.Abs(moduleRoot)
	if err != nil {
		t.Fatalf("abs module root: %v", err)
	}
	tmpDir, err := os.MkdirTemp("", "sandbox-bundle-*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tarPath := filepath.Join(tmpDir, "sandbox.tar")
	tarFile, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("create sandbox.tar: %v", err)
	}
	saveCmd := exec.CommandContext(ctx, "docker", "save", agentImage)
	saveCmd.Stdout = tarFile
	var saveErr bytes.Buffer
	saveCmd.Stderr = &saveErr
	if runErr := saveCmd.Run(); runErr != nil {
		tarFile.Close()
		t.Fatalf("docker save %s: %v\n%s", agentImage, runErr, saveErr.String())
	}
	tarFile.Close()

	buildCmd := exec.CommandContext(ctx, "docker", "build",
		"-f", filepath.Join(absRoot, "docker/bundler.Dockerfile"),
		"--build-context", "sandbox-tar="+tmpDir,
		"-t", bundleTag,
		absRoot)
	var buildOut bytes.Buffer
	buildCmd.Stdout, buildCmd.Stderr = &buildOut, &buildOut
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("bundle %s: %v\n%s", agentImage, err, buildOut.String())
	}
}

// BuildAgentWebsocketBundle builds the agent-websocket fixture image from
// test/e2e/fixtures/agent-websocket/ and bundles it into a sandbox-ready image.
// Returns the bundle image tag.
func BuildAgentWebsocketBundle(t *testing.T) string {
	t.Helper()
	fixtureDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures/agent-websocket"))
	if err != nil {
		t.Fatalf("abs agent-websocket fixture dir: %v", err)
	}
	const agentImage = "sandbox-agent-websocket:e2e"
	const bundleTag = "sandbox-bundle-agent-websocket:e2e"
	BuildImages(t, filepath.Join(fixtureDir, "Dockerfile"), fixtureDir, agentImage)
	BuildSandboxBundle(t, agentImage, bundleTag)
	return bundleTag
}

// BuildAgentNodeBundle builds the agent-node fixture image from
// test/e2e/fixtures/agent-node/ and bundles it into a sandbox-ready image.
// Returns the bundle image tag.
func BuildAgentNodeBundle(t *testing.T) string {
	t.Helper()
	fixtureDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures/agent-node"))
	if err != nil {
		t.Fatalf("abs agent-node fixture dir: %v", err)
	}
	const agentImage = "sandbox-agent-node:e2e"
	const bundleTag = "sandbox-bundle-agent-node:e2e"
	BuildImages(t, filepath.Join(fixtureDir, "Dockerfile"), fixtureDir, agentImage)
	BuildSandboxBundle(t, agentImage, bundleTag)
	return bundleTag
}

// BuildTSMCPServerBundle builds the TypeScript MCP server image from
// client/typescript/examples/mcp-server/image/ and bundles it into a
// sandbox-ready image. Returns the bundle image tag.
func BuildTSMCPServerBundle(t *testing.T) string {
	t.Helper()
	imageDir, err := filepath.Abs(filepath.Join(moduleRoot, "client/typescript/examples/mcp-server/image"))
	if err != nil {
		t.Fatalf("abs ts mcp-server image dir: %v", err)
	}
	const agentImage = "ts-mcp-server:e2e"
	const bundleTag = "ts-mcp-server-bundle:e2e"
	BuildImages(t, filepath.Join(imageDir, "Dockerfile"), imageDir, agentImage)
	BuildSandboxBundle(t, agentImage, bundleTag)
	return bundleTag
}
