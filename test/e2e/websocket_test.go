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

// TestWebSocketE2E verifies that the transparent egress proxy correctly
// tunnels WebSocket (ws://) connections:
//
//   - An allowed ws:// upgrade to upstream-ws:17082 reaches the host-side echo
//     server, frames are forwarded bidirectionally, and the proxy emits an
//     egress.request event with access="allowed".
//   - A ws:// upgrade to upstream-denied:17082 is rejected with 403 before the
//     upstream is ever dialled, producing an egress.request event with
//     access="denied".
func TestWebSocketE2E(t *testing.T) {
	setup.RequireDocker(t)
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	bundleImage := setup.BuildAgentWebsocketBundle(t)
	stopEcho := setup.StartWSEchoServer(t)
	defer stopEcho()

	key := fmt.Sprintf("e2e-ws-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image: bundleImage,
		ExtraHosts: []string{
			"upstream-ws:host-gateway",
			"upstream-denied:host-gateway",
		},
		Egress: []hiverclient.EgressRule{
			{Access: "allow", Host: "upstream-ws", Ports: []int{setup.WSEchoPort}},
		},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))
	t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}

	// Collect all sandbox events via the Go client. Once the agent prints
	// DONE (after which it enters an infinite sleep loop) we give the proxy a
	// brief settle window then stop watching.
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	eventsCh, _ := sbx.WatchEvents(watchCtx, 0)

	var collected []hiverclient.SandboxEvent
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for ev := range eventsCh {
			collected = append(collected, ev)
			if ev.Type == "stdio" && strings.Contains(ev.Stdout, "DONE") {
				time.Sleep(300 * time.Millisecond) // let proxy flush final audit events
				watchCancel()
			}
		}
	}()

	select {
	case <-collectDone:
	case <-time.After(60 * time.Second):
		watchCancel()
		<-collectDone
		t.Fatalf("timed out waiting for agent DONE; collected %d events", len(collected))
	}

	// Verify egress.request events from the proxy.
	var allowedWS, deniedWS bool
	for _, ev := range collected {
		if ev.Type != "egress.request" {
			continue
		}
		if ev.Host == "upstream-ws" && ev.Access == "allowed" {
			allowedWS = true
		}
		if ev.Host == "upstream-denied" && ev.Access == "denied" {
			deniedWS = true
		}
	}
	if !allowedWS {
		t.Errorf("egress.request: no allowed event for upstream-ws")
	}
	if !deniedWS {
		t.Errorf("egress.request: no denied event for upstream-denied")
	}

	// Verify agent output via stdio events.
	var sawAllowedOK, sawDeniedOK bool
	for _, ev := range collected {
		if ev.Type != "stdio" {
			continue
		}
		if strings.Contains(ev.Stdout, "WS ALLOWED OK") {
			sawAllowedOK = true
		}
		if strings.Contains(ev.Stdout, "WS DENIED OK") {
			sawDeniedOK = true
		}
	}
	if !sawAllowedOK {
		t.Errorf("agent stdout: WS ALLOWED OK not seen")
	}
	if !sawDeniedOK {
		t.Errorf("agent stdout: WS DENIED OK not seen")
	}

	if t.Failed() {
		t.Logf("collected %d events:", len(collected))
		for _, ev := range collected {
			switch ev.Type {
			case "egress.request":
				t.Logf("  egress.request host=%s access=%s", ev.Host, ev.Access)
			case "stdio":
				if ev.Stdout != "" {
					t.Logf("  stdio stdout=%q", ev.Stdout)
				}
				if ev.Stderr != "" {
					t.Logf("  stdio stderr=%q", ev.Stderr)
				}
			}
		}
	}
}
