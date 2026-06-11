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

// TestFSEventsE2E verifies the fs.request / fs.response event pair emitted
// by sbxfuse. Two operations are triggered via Exec:
//   - an ALLOWED write to /workspace/fsevents-write.txt → fs.request(allowed,write)
//     + fs.response(backend:local)
//   - a DENIED read from /workspace/secret/keys.txt → fs.request(denied,read),
//     no paired response (ACL deny short-circuits before the backend is contacted)
//
// Events are collected via WatchEvents after the Exec calls complete; the
// broker replays every past event when lastEventID=0, so the test is race-free.
func TestFSEventsE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	key := fmt.Sprintf("e2e-fs-events-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "hiversh/python:3.13-alpine",
		Entrypoint: "tail -f /dev/null",
		FS: []hiverclient.FileSystem{{
			Mount:   "/workspace",
			Backend: "local",
			ACLs: []hiverclient.ACLRule{
				{Path: "/workspace", Access: "rw"},
				{Path: "/workspace/**", Access: "rw"},
				{Path: "/workspace/secret/**", Access: "deny"},
			},
		}},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))
	t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}

	const writePath = "/workspace/fsevents-write.txt"
	const secretPath = "/workspace/secret/keys.txt"

	// Trigger an ALLOWED write: open the file for writing via FUSE. sbxfuse
	// evaluates the ACL (rw on /workspace/**), permits the op, emits
	// fs.request(allowed, write) and fs.response(backend:local).
	if _, err := sbx.Exec(ctx, hiverclient.ExecRequest{
		Command: fmt.Sprintf(`python3 -c "open('%s', 'w').write('hello from e2e')"`, writePath),
	}); err != nil {
		t.Fatalf("Exec write: %v", err)
	}

	// Trigger a DENIED read: open a path under /workspace/secret/**. sbxfuse
	// evaluates the ACL (deny on /workspace/secret/**) and short-circuits with
	// ENOENT, emitting fs.request(denied, read). No fs.response is emitted
	// because the backend is never contacted.
	if _, err := sbx.Exec(ctx, hiverclient.ExecRequest{
		Command: fmt.Sprintf(`python3 -c "
try:
    open('%s', 'r').read()
except Exception:
    pass  # ENOENT from FUSE deny — expected
"`, secretPath),
	}); err != nil {
		t.Fatalf("Exec denied read: %v", err)
	}

	// Subscribe from id=0: the broker replays all past events so both operations
	// above are visible even though they finished before we subscribed.
	events := collectSandboxEvents(t, sbx, ctx, 5*time.Second)
	t.Logf("collected %d events", len(events))

	fsReqs := filterByType(events, "fs.request")
	fsResps := filterByType(events, "fs.response")

	t.Run("write_allowed_fields", func(t *testing.T) {
		ev := findEvent(fsReqs, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "allowed" && e.Operation == "write" &&
				strings.Contains(e.Path, "fsevents-write.txt")
		})
		if ev == nil {
			t.Fatalf("no fs.request{access:allowed, operation:write, path~fsevents-write.txt}; events:\n%s",
				summarizeFSReqs(fsReqs))
		}
		if ev.Mount != "/workspace" {
			t.Errorf("fs.request mount=%q, want /workspace", ev.Mount)
		}
		if ev.ID == 0 {
			t.Errorf("fs.request id=0, want >0")
		}
		if ev.Timestamp == "" {
			t.Errorf("fs.request timestamp empty")
		}
	})

	t.Run("write_response_paired", func(t *testing.T) {
		allowed := findEvent(fsReqs, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "allowed" && e.Operation == "write" &&
				strings.Contains(e.Path, "fsevents-write.txt")
		})
		if allowed == nil {
			t.Skip("allowed fs.request not found; skipping response pairing check")
		}
		resp := findEvent(fsResps, func(e hiverclient.SandboxEvent) bool {
			return e.RequestID == allowed.ID
		})
		if resp == nil {
			t.Fatalf("no fs.response with request_id=%d (write request id); responses:\n%s",
				allowed.ID, summarizeFSResps(fsResps))
		}
		if resp.Backend != "local" {
			t.Errorf("fs.response backend=%q, want local", resp.Backend)
		}
		if resp.DurationMs < 0 {
			t.Errorf("fs.response duration_ms=%d, want >=0", resp.DurationMs)
		}
		if resp.ID == 0 {
			t.Errorf("fs.response id=0, want >0")
		}
		if resp.Timestamp == "" {
			t.Errorf("fs.response timestamp empty")
		}
	})

	t.Run("denied_read_fields", func(t *testing.T) {
		// The FUSE deny fires at the first node under /workspace/secret that
		// the kernel looks up (typically the directory itself, so the path is
		// /workspace/secret rather than the full file path). Use HasPrefix to
		// match either the directory node or a deeper file path.
		ev := findEvent(fsReqs, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "denied" && e.Operation == "read" &&
				strings.HasPrefix(e.Path, "/workspace/secret")
		})
		if ev == nil {
			t.Fatalf("no fs.request{access:denied, operation:read, path prefix /workspace/secret}; events:\n%s",
				summarizeFSReqs(fsReqs))
		}
		if ev.Mount != "/workspace" {
			t.Errorf("denied fs.request mount=%q, want /workspace", ev.Mount)
		}
		if ev.ID == 0 {
			t.Errorf("denied fs.request id=0, want >0")
		}
		if ev.Timestamp == "" {
			t.Errorf("denied fs.request timestamp empty")
		}
	})

	t.Run("denied_read_response_no_error", func(t *testing.T) {
		// sbxfuse always emits a paired fs.response for every denied fs.request
		// (for symmetry — every fs.request has a response). But because the ACL
		// layer short-circuits before the backend is contacted, the response has
		// no error: a non-nil error would mean a backend failure, not a policy
		// denial. Verify the response exists and its error field is absent.
		denied := findEvent(fsReqs, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "denied" && strings.HasPrefix(e.Path, "/workspace/secret")
		})
		if denied == nil {
			t.Skip("denied fs.request not found; skipping response check")
		}
		resp := findEvent(fsResps, func(e hiverclient.SandboxEvent) bool {
			return e.RequestID == denied.ID
		})
		if resp == nil {
			t.Fatalf("no fs.response paired with denied fs.request id=%d", denied.ID)
		}
		if resp.Error != "" {
			t.Errorf("denied fs.response error=%q, want empty (ACL denial is not a backend error)", resp.Error)
		}
	})
}

func summarizeFSReqs(events []hiverclient.SandboxEvent) string {
	var out string
	for _, e := range events {
		out += fmt.Sprintf("  id=%d access=%s operation=%s mount=%s path=%s\n",
			e.ID, e.Access, e.Operation, e.Mount, e.Path)
	}
	return out
}

func summarizeFSResps(events []hiverclient.SandboxEvent) string {
	var out string
	for _, e := range events {
		out += fmt.Sprintf("  id=%d request_id=%d backend=%s duration_ms=%d error=%s\n",
			e.ID, e.RequestID, e.Backend, e.DurationMs, e.Error)
	}
	return out
}
