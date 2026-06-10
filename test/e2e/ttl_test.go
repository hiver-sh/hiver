package e2e_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestTTLE2E exercises the /v1/ping keepalive contract against the ttl
// fixture. The sandbox is launched via the `hiver` CLI (`hiver start`),
// which builds, bundles, and provisions it on the gateway — the test
// never shells out to Docker directly. Assumes `hiver up` is running.
//
//  1. While pings flow every ttl/3, the sandbox must stay reachable.
//  2. Once pings stop, the sandbox must shut itself down within ttl+30s.
func TestTTLE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	const (
		ttlSeconds  = 5
		ttlDuration = ttlSeconds * time.Second
	)

	// The ttl fixture reuses the mcp-server image — a Debian box whose
	// entrypoint is `sleep infinity`, so it stays up between pings. Hand
	// the fixture directory to `hiver start`, which builds + bundles it.
	imageDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures/mcp-server"))
	if err != nil {
		t.Fatalf("abs image dir: %v", err)
	}

	key := fmt.Sprintf("e2e-ttl-%d", time.Now().UnixNano())

	// Start the sandbox through the CLI — no direct Docker calls. `hiver
	// start` bundles the image, provisions the sandbox on the gateway,
	// and blocks until it's reachable. Bundling can build an image on the
	// first run, so give it a generous deadline.
	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelStart()
	startCmd := exec.CommandContext(startCtx, "hiver", "start", key,
		"--image", imageDir,
		"--ttl", fmt.Sprint(ttlSeconds),
		"--gateway-url", setup.GatewayURL,
	)
	if out, err := startCmd.CombinedOutput(); err != nil {
		t.Fatalf("hiver start: %v\n%s", err, out)
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))
	t.Cleanup(func() {
		// Best-effort: the sandbox may have already shut itself down.
		_ = exec.Command("hiver", "stop", key, "--gateway-url", setup.GatewayURL).Run()
	})

	// Resolve a handle to the freshly started sandbox so we can ping it.
	sbx := findSandbox(t, c, key)
	if sbx == nil {
		t.Fatalf("sandbox %q not listed after hiver start", key)
	}

	// Phase 1: keepalive. Send pings every ttl/3; sandbox must stay
	// reachable throughout the 3*ttl window.
	keepaliveEnd := time.Now().Add(3 * ttlDuration)
	for time.Now().Before(keepaliveEnd) {
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := sbx.Ping(pingCtx)
		cancel()
		if err != nil {
			t.Fatalf("sandbox stopped responding to ping during keepalive: %v", err)
		}
		time.Sleep(ttlDuration / 3)
	}

	// Phase 2: stop pinging. The idle timer now runs down and the sandbox
	// must self-terminate within ttl+30s. Detect that by polling the
	// controller's sandbox listing — crucially NOT by pinging the sandbox:
	// every request to the sandbox API resets its TTL (see the middleware in
	// sandbox_server.go), so a sub-TTL poll-ping would keep it alive forever.
	// findSandbox hits /controller (a different gateway route), so it never
	// touches the countdown.
	shutdownDeadline := time.Now().Add(ttlDuration + 30*time.Second)
	for time.Now().Before(shutdownDeadline) {
		if findSandbox(t, c, key) == nil {
			return // controller no longer lists it — it shut itself down
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("sandbox still listed %v after pings stopped", ttlDuration+30*time.Second)
}

// findSandbox returns the sandbox with the given key as the gateway lists
// it, or nil if it isn't present (e.g. it has shut itself down).
func findSandbox(t *testing.T, c *hiverclient.Client, key string) *hiverclient.Sandbox {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sandboxes, err := c.ListSandboxes(ctx)
	if err != nil {
		t.Logf("list sandboxes: %v", err)
		return nil
	}
	for _, s := range sandboxes {
		if s.Key == key {
			return s
		}
	}
	return nil
}
