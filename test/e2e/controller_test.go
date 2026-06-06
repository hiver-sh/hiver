package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	controllergen "github.com/blasten/hive/internal/api/gen/controller"
	"github.com/blasten/hive/test/e2e/setup"
)

const (
	composeFile      = "../../docker/compose.yaml"
	controllerURL    = "http://127.0.0.1:9000"
	sandboxImage     = "hive-sandbox-bundler"
	composeProjectID = "hive"
)

// TestControllerGetOrCreateSandboxE2E exercises the control plane's
// PUT /v1/sandboxes/{id} contract end-to-end. It boots the `hive`
// compose stack, calls the API twice with the same id (expecting
// 201 then 200 with the same record), checks that the spawned
// container is labeled into the hive project, and verifies a
// malformed body produces 400.
//
// The full sandbox lifecycle (sandboxd booting, agent reaching DONE,
// audit events) is covered by the per-fixture tests; this test only
// asserts the controller's surface.
func TestControllerGetOrCreateSandboxE2E(t *testing.T) {
	setup.RequireDocker(t)

	// Build the component images then package them into the sandbox bundle.
	dockerCompose(t, "--profile", "build", "build", "hive-sandbox-runtime", "hive-mcp-server")
	setup.BuildSandboxBundle(t, "hive-mcp-server", sandboxImage)

	// Bring up the controller (only service in the default profile).
	// `up -d --build` rebuilds the controller image so this test
	// reflects the current source even when the developer hasn't
	// run `make up` recently.
	dockerCompose(t, "up", "-d", "--build", "hive-controller")
	t.Cleanup(func() {
		// Sweep sandbox containers first: `compose down --remove-orphans`
		// only stops services declared in compose.yaml plus orphans that
		// match its strict label set. Our sandboxes carry an extra
		// `hive.sandbox.id` label and slip past orphan detection, leaving
		// the network un-removable. Force-remove by id label, then down.
		removeSandboxContainers(t)
		dockerCompose(t, "down", "-v", "--remove-orphans")
	})

	waitForController(t, controllerURL)

	id := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	body := fmt.Appendf(nil, `{
		"image": %q,
		"fs": [{
			"mount": "/workspace",
			"backend": "local",
			"acls": [
				{"path": "/workspace",    "access": "rw"},
				{"path": "/workspace/**", "access": "rw"}
			]
		}]
	}`, sandboxImage)

	// First PUT → 201 Created with the new record.
	created, status := putSandbox(t, controllerURL, id, body)
	if status != http.StatusCreated {
		t.Fatalf("first PUT: status %d, want 201", status)
	}
	if created.Key != id {
		t.Errorf("sandbox.key = %q, want %q", created.Key, id)
	}

	// The container exists on the host daemon and is grouped into
	// the hive compose project.
	containerName := fmt.Sprintf("%s-sandbox-%s", composeProjectID, id)
	label := dockerInspectLabel(t, containerName, "com.docker.compose.project")
	if label != composeProjectID {
		t.Errorf("container %s compose.project label = %q, want %q",
			containerName, label, composeProjectID)
	}

	// The sandbox's per-pod API server (sandboxd) is reachable via the
	// gateway at /sandbox/{id}/. Round-trip GET /v1/config and assert
	// the response carries the workspace mount we passed in — that
	// proves both that sandboxd booted and that it loaded the spec.
	sandboxBase := controllerURL + "/sandbox/" + id
	cfg := sandboxGetConfig(t, sandboxBase, 60*time.Second)
	if !strings.Contains(cfg, `"mount":"/workspace"`) {
		t.Errorf("sandbox /v1/config: missing /workspace mount; got %s", cfg)
	}

	// A second id with a malformed body (missing required `fs`) is
	// rejected by the OpenAPI validator before the handler runs.
	_, badStatus := putSandboxRaw(t, controllerURL, "e2e-bad-"+id, []byte(`{"image":"x"}`))
	if badStatus != http.StatusBadRequest {
		t.Errorf("malformed body: status %d, want 400", badStatus)
	}
}

// sandboxGetConfig polls GET /v1/config against the sandbox's per-pod
// API server until it answers 200, then returns the body. The endpoint
// is host-published, so we hit it directly. Boot is slow (sandbox.tar
// unpack + sandboxd init + FUSE mount) — bound the wait at `deadline`.
func sandboxGetConfig(t *testing.T, endpoint string, deadline time.Duration) string {
	t.Helper()
	end := time.Now().Add(deadline)
	var lastErr error
	var lastStatus int
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(end) {
		resp, err := client.Get(endpoint + "/v1/config")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			lastStatus = resp.StatusCode
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("GET %s/v1/config never returned 200: lastStatus=%d lastErr=%v", endpoint, lastStatus, lastErr)
	return ""
}

// dockerCompose runs `docker compose -f docker/compose.yaml <args>`
// and fails the test on non-zero exit. Output is surfaced on failure
// so a missing image / port collision is easy to diagnose.
func dockerCompose(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"compose", "-f", composeFile}, args...)
	out, err := exec.Command("docker", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(full, " "), err, out)
	}
}

// waitForController polls the controller's port until it answers an
// HTTP request (any status — we just want a TCP-level handshake +
// response). The image build inside `up -d --build` can take a while
// on a cold cache; bound the wait at 90s.
func waitForController(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		// Any path works: the validator rejects unknown routes with
		// 404 long before we'd get a connection error.
		resp, err := http.Get(baseURL + "/v1/sandboxes/probe")
		if err == nil {
			resp.Body.Close()
			return
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("controller not reachable at %s within 90s: %v", baseURL, lastErr)
}

func putSandbox(t *testing.T, baseURL, id string, body []byte) (controllergen.Sandbox, int) {
	t.Helper()
	raw, status := putSandboxRaw(t, baseURL, id, body)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("PUT /v1/sandboxes/%s: status %d body %s", id, status, raw)
	}
	var sb controllergen.Sandbox
	if err := json.Unmarshal(raw, &sb); err != nil {
		t.Fatalf("decode sandbox: %v: %s", err, raw)
	}
	return sb, status
}

func putSandboxRaw(t *testing.T, baseURL, id string, body []byte) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, baseURL+"/v1/sandboxes/"+id, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build PUT: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", id, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode
}

func removeSandboxContainers(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=hive.sandbox.id").Output()
	if err != nil {
		t.Logf("list sandbox containers: %v", err)
		return
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return
	}
	args := append([]string{"rm", "-f"}, ids...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Logf("remove sandbox containers: %v\n%s", err, out)
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
