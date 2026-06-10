package e2e_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestEventsLastEventIdE2E exercises `GET /v1/events` end-to-end and
// verifies resume semantics. It provisions the agent-node fixture via
// the hiver controller and subscribes to its event stream through the
// gateway. Assumes `hiver up` is running.
func TestEventsLastEventIdE2E(t *testing.T) {
	setup.RequireDocker(t)
	setup.RequireStack(t)

	fixtureDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures/agent-node"))
	if err != nil {
		t.Fatalf("abs fixture dir: %v", err)
	}
	dockerfile := filepath.Join(fixtureDir, "Dockerfile")

	agentImage := "sandbox-agent-node:e2e"
	bundleImage := "sandbox-bundle-agent-node:e2e"
	setup.BuildImages(t, dockerfile, fixtureDir, agentImage)
	setup.BuildSandboxBundle(t, agentImage, bundleImage)

	ctx := context.Background()
	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))

	key := fmt.Sprintf("e2e-events-%d", time.Now().UnixNano())
	cfg := hiverclient.SandboxConfig{
		Image: bundleImage,
		FS: []hiverclient.FileSystem{{
			Mount:   "/workspace",
			Backend: "local",
			ACLs: []hiverclient.ACLRule{
				{Path: "/workspace", Access: "rw"},
				{Path: "/workspace/**", Access: "rw"},
				{Path: "/workspace/inputs/**", Access: "ro"},
				{Path: "/workspace/secret/**", Access: "deny"},
			},
		}},
		Egress: []hiverclient.EgressRule{
			{
				Access:  "allow",
				Host:    "upstream-allowed",
				Methods: []string{"GET"},
				Paths:   []string{"/"},
				Override: &hiverclient.EgressOverride{
					Headers: map[string]string{"X-Sandbox-Agent": "agent-python"},
				},
			},
			{
				Access:  "allow",
				Host:    "go.dev",
				Methods: []string{"GET"},
				Paths:   []string{"/solutions/case-studies/*"},
			},
		},
	}

	sbx, err := c.GetOrCreateSandbox(ctx, key, cfg)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Shutdown(context.Background(), key)
	})

	// Subscribe to events via the gateway's /sandbox/{key}/ route, which
	// Envoy forwards to the sandboxd API inside the container.
	baseURL := setup.GatewayURL + "/sandbox/" + sbx.Key
	idleTimeout := 750 * time.Millisecond

	// Collect the full event stream. The agent-node fixture's HTTP server
	// keeps the sandbox alive, so the SSE stream stays open; the idle
	// timeout fires once the agent's work is done and no new events arrive.
	full := setup.FetchEvents(t, baseURL, setup.FetchOpts{
		LastEventID: "0",
		IdleTimeout: idleTimeout,
	})
	t.Logf("SSE events (%d):\n%s", len(full), setup.SummarizeEvents(full))

	if len(full) < 4 {
		t.Fatalf("expected ≥4 events to exercise resume; got %d", len(full))
	}
	assertIDsMonotonic(t, "full", full)

	// Verify the DONE marker arrived in a stdio event.
	sawDone := false
	for _, e := range full {
		if typ, _ := e["type"].(string); typ == "stdio" {
			if s, ok := e["stdout"].(string); ok && strings.Contains(s, "DONE") {
				sawDone = true
				break
			}
		}
	}
	if !sawDone {
		t.Errorf("stdio: no event with 'DONE' in stdout; got %d events total", len(full))
	}

	// Verify fs events are present (FUSE operations are always sync, so
	// these are independent of network/egress availability).
	fsCount := 0
	for _, e := range full {
		if typ, _ := e["type"].(string); typ == "fs.request" {
			fsCount++
		}
	}
	if fsCount == 0 {
		t.Errorf("expected fs.request events; got none")
	}

	// Pick the midpoint event's id and use it as the resume point.
	midID := eventID(t, full[len(full)/2])

	afterQuery := setup.FetchEvents(t, baseURL, setup.FetchOpts{
		LastEventID: strconv.FormatInt(midID, 10),
		IdleTimeout: idleTimeout,
	})
	assertResumeMatchesSuffix(t, "?lastEventId resume", full, afterQuery, midID)

	afterHeader := setup.FetchEvents(t, baseURL, setup.FetchOpts{
		LastEventIDHeader: strconv.FormatInt(midID, 10),
		IdleTimeout:       idleTimeout,
	})
	assertResumeMatchesSuffix(t, "Last-Event-ID header resume", full, afterHeader, midID)

	// A lastEventId past the highest emitted id should drain nothing.
	beyondID := eventID(t, full[len(full)-1]) + 1_000_000
	beyond := setup.FetchEvents(t, baseURL, setup.FetchOpts{
		LastEventID: strconv.FormatInt(beyondID, 10),
		IdleTimeout: 300 * time.Millisecond,
	})
	if len(beyond) != 0 {
		t.Errorf("resume past max: expected 0 events, got %d", len(beyond))
	}
}

// eventID extracts the monotonic id stamped by the broker. JSON
// unmarshals numbers as float64; the broker emits int64-fit values so
// the conversion is exact.
func eventID(t *testing.T, ev map[string]any) int64 {
	t.Helper()
	v, ok := ev["id"]
	if !ok {
		t.Fatalf("event missing id: %v", ev)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("event id is %T, want number: %v", v, ev)
	}
	return int64(f)
}

func assertIDsMonotonic(t *testing.T, label string, events []map[string]any) {
	t.Helper()
	for i := 1; i < len(events); i++ {
		prev, cur := eventID(t, events[i-1]), eventID(t, events[i])
		if cur <= prev {
			t.Errorf("%s: ids not strictly increasing at index %d: %d -> %d", label, i, prev, cur)
			return
		}
	}
}

// assertResumeMatchesSuffix asserts that `resumed` is exactly the
// subset of `full` with id > after. Allow `resumed` to be a prefix of
// the live tail too: post-DONE the agent is quiescent and shouldn't
// emit new events, but a single late stdio line wouldn't be a bug —
// we only care that everything in `resumed` was *in* `full` and that
// the suffix we expected is present.
func assertResumeMatchesSuffix(t *testing.T, label string, full, resumed []map[string]any, after int64) {
	t.Helper()
	wantIDs := map[int64]bool{}
	for _, e := range full {
		if id := eventID(t, e); id > after {
			wantIDs[id] = true
		}
	}
	gotIDs := map[int64]bool{}
	for _, e := range resumed {
		id := eventID(t, e)
		if id <= after {
			t.Errorf("%s: got event id=%d, expected only id>%d", label, id, after)
		}
		gotIDs[id] = true
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("%s: missing event id=%d (expected in resume after %d)", label, id, after)
		}
	}
	if t.Failed() {
		t.Logf("%s: want ids %v, got ids %v", label, sortedKeys(wantIDs), sortedKeys(gotIDs))
	}
}

func sortedKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
