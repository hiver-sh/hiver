package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

const (
	sandboxImage     = "hive-sandbox-bundler"
	composeProjectID = "hiver"
)

// TestControllerGetOrCreateSandboxE2E exercises the control plane's
// PUT /v1/sandboxes/{key} contract end-to-end via the hiver client SDK.
// It assumes `hiver up` has already been called — the controller and
// gateway must be running before the test starts.
//
// The test verifies: sandbox creation, container labelling, config
// round-trip, idempotent re-provision, and 400 rejection of a
// malformed request body.
func TestControllerGetOrCreateSandboxE2E(t *testing.T) {
	setup.RequireDocker(t)
	setup.RequireStack(t)

	// Build the agent image via the compose build profile, then bundle
	// it into the image the controller will run as a container.
	dockerCompose(t, "--profile", "build", "build", "core", "agent-cli-standalone")
	setup.BuildSandboxBundle(t, "hiversh/agent-cli-standalone:latest", sandboxImage)

	ctx := context.Background()
	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))

	key := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	cfg := hiverclient.SandboxConfig{
		Image: sandboxImage,
		FS: []hiverclient.FileSystem{{
			Mount:   "/workspace",
			Backend: "local",
			ACLs: []hiverclient.ACLRule{
				{Path: "/workspace", Access: "rw"},
				{Path: "/workspace/**", Access: "rw"},
			},
		}},
	}

	// First provision — creates a new sandbox and waits for it to ping.
	sbx, err := c.GetOrCreateSandbox(ctx, key, cfg)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox (create): %v", err)
	}
	t.Cleanup(func() {
		_ = c.Shutdown(context.Background(), key)
	})

	if sbx.Key != key {
		t.Errorf("sandbox.Key = %q, want %q", sbx.Key, key)
	}

	// The container must be on the host daemon and grouped under the
	// hive compose project (docker_runtime.go adds this label).
	containerName := fmt.Sprintf("%s-sandbox-%s", composeProjectID, key)
	label := dockerInspectLabel(t, containerName, "com.docker.compose.project")
	if label != composeProjectID {
		t.Errorf("container %s compose.project label = %q, want %q",
			containerName, label, composeProjectID)
	}

	// Round-trip GET /v1/config and confirm the workspace mount is present.
	cfgResp, err := sbx.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	hasMnt := false
	for _, fs := range cfgResp.FS {
		if fs.Mount == "/workspace" {
			hasMnt = true
			break
		}
	}
	if !hasMnt {
		t.Errorf("GetConfig: /workspace mount missing; got %+v", cfgResp.FS)
	}

	// Second provision with the same key must be idempotent and return
	// the same sandbox.
	sbx2, err := c.GetOrCreateSandbox(ctx, key, cfg)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox (idempotent): %v", err)
	}
	if sbx2.ID != sbx.ID {
		t.Errorf("idempotent provision returned different ID: %q vs %q", sbx2.ID, sbx.ID)
	}

	// A body that omits the required `fs` field must be rejected with 400.
	badURL := setup.GatewayURL + "/controller/v1/sandboxes/e2e-bad-" + key
	req, err := http.NewRequest(http.MethodPut, badURL, bytes.NewReader([]byte(`{"image":"x"}`)))
	if err != nil {
		t.Fatalf("build bad PUT: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("bad PUT request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body: status %d, want 400", resp.StatusCode)
	}
}

// dockerCompose runs `docker compose -f docker/compose.yaml <args>`
// and fails the test on non-zero exit.
func dockerCompose(t *testing.T, args ...string) {
	t.Helper()
	const composeFile = "../../docker/compose.yaml"
	full := append([]string{"compose", "-f", composeFile}, args...)
	out, err := exec.Command("docker", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(full, " "), err, out)
	}
}

func dockerInspectLabel(t *testing.T, container, label string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect",
		"--format", fmt.Sprintf(`{{index .Config.Labels %q}}`, label),
		container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect %s: %v\n%s", container, err, out)
	}
	return strings.TrimSpace(string(out))
}
