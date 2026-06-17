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
		Image:      "hiversh/python:3.13-alpine",
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

	if err := c.Shutdown(ctx, key); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// After shutdown the container is removed and the gateway no longer
	// routes to it. Ping must fail.
	if err := sbx.Ping(ctx); err == nil {
		t.Error("Ping after shutdown: expected error, got nil")
	}
}
