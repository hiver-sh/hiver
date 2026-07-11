package e2e_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestControllerEventsMultiTenantE2E verifies the controller's aggregated
// lifecycle stream (GET /controller/v1/sandboxes/events) surfaces the lifecycle
// of EACH inner sandbox when multiple are packed into one pod. The controller
// holds a persistent GET /v1/events connection to every pod and forwards each
// inner sandbox's transitions (start → stop → destroy), tagged with the pod's
// routing id and the inner key.
//
// Requires the controller in pack mode (HIVE_PACK=1); skips otherwise (the 1:1
// controller gives each key its own pod, so they don't share a routing id and
// the multi-tenant aggregation can't be exercised).
func TestControllerEventsMultiTenantE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	const image = "python"
	ts := time.Now().UnixNano()
	keyA := fmt.Sprintf("evta-%d", ts)
	keyB := fmt.Sprintf("evtb-%d", ts)

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(3*time.Minute))

	// Subscribe to the controller's lifecycle stream BEFORE creating anything, so
	// the create transitions are observed live. Collect, per key, the set of
	// statuses seen and the id each arrived with.
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	events, errs := c.WatchEvents(watchCtx)

	var mu sync.Mutex
	statuses := map[string]map[string]bool{} // key → set of statuses
	ids := map[string]string{}               // key → last id seen
	go func() {
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}
				mu.Lock()
				if statuses[ev.Key] == nil {
					statuses[ev.Key] = map[string]bool{}
				}
				statuses[ev.Key][ev.Status] = true
				ids[ev.Key] = ev.ID
				mu.Unlock()
			case <-errs:
				// transient stream error; the client reconnects
			case <-watchCtx.Done():
				return
			}
		}
	}()

	has := func(key, status string) bool {
		mu.Lock()
		defer mu.Unlock()
		return statuses[key][status]
	}
	idFor := func(key string) string {
		mu.Lock()
		defer mu.Unlock()
		return ids[key]
	}
	waitFor := func(key, status string, d time.Duration) bool {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if has(key, status) {
				return true
			}
			time.Sleep(200 * time.Millisecond)
		}
		return has(key, status)
	}

	// Let the stream connect (the controller discovers pods and opens its
	// per-pod connections) before provisioning.
	time.Sleep(1 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	mk := func(key string) *hiverclient.Sandbox {
		cfg := hiverclient.SandboxConfig{
			Image:      image,
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			FS: []hiverclient.FileSystem{
				{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/workspace/**", Access: "rw"}}},
			},
		}
		sbx, err := c.GetOrCreateSandbox(ctx, key, cfg)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox(%s): %v", key, err)
		}
		return sbx
	}

	sbxA := mk(keyA)
	sbxB := mk(keyB)
	// Tear each sandbox down via its own API (no controller involvement).
	t.Cleanup(func() {
		_ = sbxA.Shutdown(context.Background())
		_ = sbxB.Shutdown(context.Background())
	})

	// Two same-image keys must share ONE pod (one routing id) for this to be a
	// multi-tenant test.
	if sbxA.ID != sbxB.ID {
		t.Skipf("keys landed in different pods (%s vs %s) — controller not in pack mode (HIVE_PACK=1)", sbxA.ID, sbxB.ID)
	}
	t.Logf("both keys packed into pod %s", sbxA.ID)

	// Each inner sandbox must surface a start on the controller's stream, even
	// though they share one pod/connection.
	if !waitFor(keyA, "start", 45*time.Second) {
		t.Errorf("no start event for %s on the controller stream", keyA)
	}
	if !waitFor(keyB, "start", 45*time.Second) {
		t.Errorf("no start event for %s on the controller stream", keyB)
	}

	// The events must be tagged with the shared pod's routing id.
	if got := idFor(keyA); got != sbxA.ID {
		t.Errorf("%s event id = %q, want pod id %q", keyA, got, sbxA.ID)
	}
	if got := idFor(keyB); got != sbxB.ID {
		t.Errorf("%s event id = %q, want pod id %q", keyB, got, sbxB.ID)
	}

	// Tear down keyA only: it must surface stop, and keyB (its co-tenant in the
	// same pod) must NOT — the per-key teardown is isolated.
	if err := sbxA.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown %s: %v", keyA, err)
	}
	if !waitFor(keyA, "stop", 45*time.Second) {
		t.Errorf("no stop event for %s after shutdown", keyA)
	}
	if has(keyB, "stop") {
		t.Errorf("%s got an unexpected stop event when only %s was shut down", keyB, keyA)
	}
}
