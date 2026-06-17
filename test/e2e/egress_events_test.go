package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestEgressEventsE2E verifies the egress.request / egress.response event
// pair emitted by sbxproxy. Two requests are triggered via Exec:
//   - an ALLOWED GET to 127.0.0.1:8099 (sandboxd's own API, explicitly
//     permitted in the egress rules) → egress.request(allowed) + egress.response
//   - a DENIED GET to 1.1.1.1 (not on the allowlist) → egress.request(denied),
//     no paired response
//
// All events are collected via WatchEvents after the Exec calls complete;
// the broker replays every past event when the subscription is opened with
// lastEventID=0, so the timing is race-free.
func TestEgressEventsE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	key := fmt.Sprintf("e2e-egress-events-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "hiversh/python:3.13-alpine",
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		FS: []hiverclient.FileSystem{
			{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
		},
		Egress: []hiverclient.EgressRule{
			{Access: "allow", Host: "127.0.0.1", Ports: []int{8099}},
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

	// Trigger an ALLOWED egress: GET /v1/ping on 127.0.0.1:8099 (sandboxd's API).
	// iptables redirects this outbound TCP connection through sbxproxy, which
	// permits it, forwards the request, and emits egress.request + egress.response.
	if _, err := sbx.Exec(ctx, hiverclient.ExecRequest{
		Command: `python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8099/v1/ping')"`,
	}); err != nil {
		t.Fatalf("Exec allowed request: %v", err)
	}

	// Trigger a DENIED egress: GET / on 1.1.1.1 (not in the allowlist). sbxproxy
	// intercepts the TCP connection, reads the HTTP Host header, finds no matching
	// allow rule, returns 403, and emits egress.request(denied). No response event.
	if _, err := sbx.Exec(ctx, hiverclient.ExecRequest{
		Command: `python3 -c "
import urllib.request, urllib.error
try:
    urllib.request.urlopen('http://1.1.1.1', timeout=5)
except urllib.error.HTTPError:
    pass  # 403 from sbxproxy — expected
except Exception:
    pass  # DNS or timeout — still generates deny event
"`,
	}); err != nil {
		t.Fatalf("Exec denied request: %v", err)
	}

	// Subscribe from id=0: the broker replays all past events, so both requests
	// above are visible even though they completed before this call.
	events := collectSandboxEvents(t, sbx, ctx, 5*time.Second)
	t.Logf("collected %d events", len(events))

	egressReqs := filterByType(events, "egress.request")
	egressResps := filterByType(events, "egress.response")

	t.Run("allowed_request_fields", func(t *testing.T) {
		ev := findEvent(egressReqs, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "allowed" && e.Host == "127.0.0.1" &&
				e.Method == "GET" && e.Path == "/v1/ping"
		})
		if ev == nil {
			t.Fatalf("no egress.request{access:allowed, host:127.0.0.1, method:GET, path:/v1/ping}; events:\n%s",
				summarizeEgressReqs(egressReqs))
		}
		if ev.ID == 0 {
			t.Errorf("allowed egress.request: id=0, want >0")
		}
		if ev.Timestamp == "" {
			t.Errorf("allowed egress.request: timestamp empty")
		}
	})

	t.Run("allowed_response_paired", func(t *testing.T) {
		allowed := findEvent(egressReqs, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "allowed" && e.Host == "127.0.0.1" && e.Path == "/v1/ping"
		})
		if allowed == nil {
			t.Skip("allowed egress.request not found; skipping response pairing check")
		}
		resp := findEvent(egressResps, func(e hiverclient.SandboxEvent) bool {
			return e.RequestID == allowed.ID
		})
		if resp == nil {
			t.Fatalf("no egress.response with request_id=%d (allowed request id); responses: %v",
				allowed.ID, summarizeEgressResps(egressResps))
		}
		if resp.Status != 200 {
			t.Errorf("egress.response status=%d, want 200", resp.Status)
		}
		if resp.DurationMs < 0 {
			t.Errorf("egress.response duration_ms=%d, want >=0", resp.DurationMs)
		}
		if resp.ID == 0 {
			t.Errorf("egress.response id=0, want >0")
		}
		if resp.Timestamp == "" {
			t.Errorf("egress.response timestamp empty")
		}
	})

	t.Run("denied_request_fields", func(t *testing.T) {
		ev := findEvent(egressReqs, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "denied" && e.Host == "1.1.1.1"
		})
		if ev == nil {
			t.Fatalf("no egress.request{access:denied, host:1.1.1.1}; events:\n%s",
				summarizeEgressReqs(egressReqs))
		}
		if ev.Method != "GET" {
			t.Errorf("denied egress.request method=%q, want GET", ev.Method)
		}
		if ev.ID == 0 {
			t.Errorf("denied egress.request id=0, want >0")
		}
		if ev.Timestamp == "" {
			t.Errorf("denied egress.request timestamp empty")
		}
	})

	t.Run("denied_request_proxy_response", func(t *testing.T) {
		// sbxproxy emits an egress.response even for denied requests: the proxy
		// itself returns 403 to the agent, so the response event reflects the
		// proxy's rejection rather than an upstream status. Verify request_id
		// is set and status is 403 (the proxy's own Forbidden).
		denied := findEvent(egressReqs, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "denied" && e.Host == "1.1.1.1"
		})
		if denied == nil {
			t.Skip("denied egress.request not found; skipping proxy-response check")
		}
		resp := findEvent(egressResps, func(e hiverclient.SandboxEvent) bool {
			return e.RequestID == denied.ID
		})
		if resp == nil {
			t.Fatalf("no egress.response paired with denied egress.request id=%d", denied.ID)
		}
		if resp.Status != 403 {
			t.Errorf("denied egress.response status=%d, want 403 (proxy rejection)", resp.Status)
		}
	})
}

// collectSandboxEvents subscribes to all events from id=0 and drains the
// channel until no new event arrives within idleTimeout. Cancels the
// subscription before returning.
func collectSandboxEvents(t *testing.T, sbx *hiverclient.Sandbox, ctx context.Context, idleTimeout time.Duration) []hiverclient.SandboxEvent {
	t.Helper()
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	eventsCh, _ := sbx.WatchEvents(watchCtx, 0)

	var out []hiverclient.SandboxEvent
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	for {
		select {
		case ev, ok := <-eventsCh:
			if !ok {
				return out
			}
			out = append(out, ev)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleTimeout)
		case <-idle.C:
			return out
		case <-ctx.Done():
			return out
		}
	}
}

func filterByType(events []hiverclient.SandboxEvent, typ string) []hiverclient.SandboxEvent {
	var out []hiverclient.SandboxEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func findEvent(events []hiverclient.SandboxEvent, match func(hiverclient.SandboxEvent) bool) *hiverclient.SandboxEvent {
	for i := range events {
		if match(events[i]) {
			return &events[i]
		}
	}
	return nil
}

func summarizeEgressReqs(events []hiverclient.SandboxEvent) string {
	var out string
	for _, e := range events {
		out += fmt.Sprintf("  id=%d access=%s host=%s method=%s path=%s\n",
			e.ID, e.Access, e.Host, e.Method, e.Path)
	}
	return out
}

func summarizeEgressResps(events []hiverclient.SandboxEvent) string {
	var out string
	for _, e := range events {
		out += fmt.Sprintf("  id=%d request_id=%d status=%d duration_ms=%d\n",
			e.ID, e.RequestID, e.Status, e.DurationMs)
	}
	return out
}
