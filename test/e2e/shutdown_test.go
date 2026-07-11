package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestShutdownE2E provisions a sandbox, confirms it is reachable via Ping,
// calls Shutdown, then asserts that Ping fails — proving the container is
// gone and the gateway no longer routes to it.
func TestShutdownE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	key := fmt.Sprintf("e2e-shutdown-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "python",
		Entrypoint: []string{"tail", "-f", "/dev/null"},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}

	// Confirm the sandbox is alive before we shut it down.
	if err := sbx.Ping(ctx); err != nil {
		t.Fatalf("Ping before shutdown: %v", err)
	}

	if err := sbx.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Shutdown is asynchronous: DELETE returns as soon as teardown is scheduled
	// (the supervisor cancels the sandbox's context and frees the slot in a
	// background goroutine), so the container may stay briefly reachable. Poll
	// until Ping fails, proving the container is gone and the gateway no longer
	// routes to it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := sbx.Ping(ctx); err != nil {
			break // torn down — expected
		}
		if time.Now().After(deadline) {
			t.Error("Ping after shutdown: still reachable after 30s, expected teardown")
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
}
