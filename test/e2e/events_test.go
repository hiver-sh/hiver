package e2e_test

import (
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/sandbox-platform/agent-sandbox/test/e2e/setup"
)

// TestEventsLastEventIdE2E exercises `GET /v1/events` end-to-end and
// verifies resume semantics.
func TestEventsLastEventIdE2E(t *testing.T) {
	setup.RunFixtureE2EHook(t, "agent-node", func(t *testing.T, baseURL string) {
		idleTimeout := 750 * time.Millisecond

		full := setup.FetchEvents(t, baseURL, setup.FetchOpts{
			LastEventID: "0",
			IdleTimeout: idleTimeout,
		})
		t.Logf("SSE events (%d):\n%s", len(full), setup.SummarizeEvents(full))
		if len(full) < 4 {
			t.Fatalf("expected ≥4 events to exercise resume; got %d", len(full))
		}
		assertIDsMonotonic(t, "full", full)
		assertEventTypes(t, full)

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

		// A lastEventId past the highest emitted id should drain
		// nothing on the replay; the live tail won't produce anything
		// because the agent is past DONE.
		beyondID := eventID(t, full[len(full)-1]) + 1_000_000
		beyond := setup.FetchEvents(t, baseURL, setup.FetchOpts{
			LastEventID: strconv.FormatInt(beyondID, 10),
			IdleTimeout: 300 * time.Millisecond,
		})
		if len(beyond) != 0 {
			t.Errorf("resume past max: expected 0 events, got %d: %v", len(beyond), beyond)
		}
	})
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

// assertEventTypes verifies the SSE stream carries every SandboxEvent
// variant the agent-node fixture drives (per its expectations.yaml):
func assertEventTypes(t *testing.T, events []map[string]any) {
	t.Helper()
	byType := map[string][]map[string]any{}
	for _, e := range events {
		typ, _ := e["type"].(string)
		byType[typ] = append(byType[typ], e)
	}

	for _, want := range []string{"egress.request", "egress.response", "fs.request", "fs.response", "stdio"} {
		if len(byType[want]) == 0 {
			t.Errorf("expected ≥1 event of type %q; got types %v", want, typeKeys(byType))
		}
	}

	// egress.request: schema-required fields + both access verdicts.
	accessCount := map[string]int{}
	for _, e := range byType["egress.request"] {
		access, _ := e["access"].(string)
		accessCount[access]++
		for _, f := range []string{"host", "method", "path"} {
			if _, ok := e[f].(string); !ok {
				t.Errorf("egress.request missing %s: %v", f, e)
			}
		}
	}
	if accessCount["allowed"] < 1 {
		t.Errorf("egress.request: expected ≥1 allowed; got %v", accessCount)
	}
	if accessCount["denied"] < 1 {
		t.Errorf("egress.request: expected ≥1 denied; got %v", accessCount)
	}

	// egress.response: schema-required fields. request_id is the
	// broker's monotonic event id of the paired egress.request event.
	for _, e := range byType["egress.response"] {
		if v, ok := e["request_id"].(float64); !ok || v <= 0 {
			t.Errorf("egress.response missing/empty request_id: %v", e)
		}
		if _, ok := e["status"].(float64); !ok {
			t.Errorf("egress.response missing status: %v", e)
		}
		if _, ok := e["duration_ms"].(float64); !ok {
			t.Errorf("egress.response missing duration_ms: %v", e)
		}
	}

	// fs.request: schema-required fields.
	for _, e := range byType["fs.request"] {
		for _, f := range []string{"mount", "path", "operation", "access"} {
			if _, ok := e[f].(string); !ok {
				t.Errorf("fs.request missing %s: %v", f, e)
			}
		}
	}

	// fs.response: schema-required backend + duration_ms. agent-node's
	// workspace mount is local, so the backend field is exactly "local".
	for _, e := range byType["fs.response"] {
		backend, _ := e["backend"].(string)
		if backend != "local" {
			t.Errorf("fs.response: expected backend=local, got %q (%v)", backend, e)
		}
		if _, ok := e["duration_ms"].(float64); !ok {
			t.Errorf("fs.response missing duration_ms: %v", e)
		}
	}

	// stdio: at least one event with a stdout chunk.
	sawStdout := false
	for _, e := range byType["stdio"] {
		if _, ok := e["stdout"].(string); ok {
			sawStdout = true
			break
		}
	}
	if !sawStdout {
		t.Errorf("stdio: no event with stdout chunk; got %d stdio events", len(byType["stdio"]))
	}
}

func typeKeys(byType map[string][]map[string]any) []string {
	out := make([]string, 0, len(byType))
	for k := range byType {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
