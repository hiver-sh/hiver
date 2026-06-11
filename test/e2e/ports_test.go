package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestPortsE2E verifies that GetPorts returns the ports declared via EXPOSE in
// the agent-node fixture's Dockerfile, and that the proxy route to the exposed
// port actually reaches the running server.
func TestPortsE2E(t *testing.T) {
	setup.RequireDocker(t)
	setup.RequireStack(t)

	bundleImage := setup.BuildAgentNodeBundle(t)

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(5*time.Minute))
	key := fmt.Sprintf("e2e-ports-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

	// Use context.Background() so the client's own Ping-wait applies
	// fresh after the Docker build finishes (build time must not eat into
	// the sandbox-start budget). Include the workspace FS mount that
	// sandboxd expects; without it the API server never comes up.
	sbx, err := c.GetOrCreateSandbox(context.Background(), key, hiverclient.SandboxConfig{
		Image: bundleImage,
		FS: []hiverclient.FileSystem{{
			Mount:   "/workspace",
			Backend: "local",
			ACLs: []hiverclient.ACLRule{
				{Path: "/workspace", Access: "rw"},
				{Path: "/workspace/**", Access: "rw"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Run("get_ports_returns_exposed_port", func(t *testing.T) {
		ports, err := sbx.GetPorts(ctx)
		if err != nil {
			t.Fatalf("GetPorts: %v", err)
		}
		var found bool
		for _, p := range ports {
			if p == 18000 {
				found = true
			}
		}
		if !found {
			t.Errorf("GetPorts=%v, want 18000", ports)
		}
	})

	t.Run("proxy_reaches_exposed_port", func(t *testing.T) {
		// The agent-node server binds synchronously before the async probe
		// loop, but sandbox startup latency means we poll until it responds.
		// POST to a non-/exec path returns 200 "ok\n".
		proxyURL := sbx.ProxyURL(18000) + "/hello"
		deadline := time.Now().Add(30 * time.Second)
		var (
			lastErr    error
			lastStatus int
		)
		for time.Now().Before(deadline) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL, strings.NewReader("ping"))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				lastErr = err
				time.Sleep(500 * time.Millisecond)
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastStatus = resp.StatusCode
			if resp.StatusCode == http.StatusOK {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		if lastStatus != 0 {
			t.Errorf("proxy POST %s: status=%d, want 200", proxyURL, lastStatus)
		} else {
			t.Errorf("proxy POST %s: no response within deadline: %v", proxyURL, lastErr)
		}
	})
}
