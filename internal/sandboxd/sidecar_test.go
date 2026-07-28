package sandboxd

import (
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
)

// TestFuseOpKind pins the fuse-op → SSE-operation bucketing. The "what
// changed" contract is that deletions surface as their own `delete`
// operation (not folded into `write`), and metadata-only probes map to
// nothing so they stay off the stream.
func TestFuseOpKind(t *testing.T) {
	cases := []struct {
		op   string
		want gen.FSRequestEventOperation
	}{
		{"read", gen.FSRequestEventOperationRead},
		{"readdir", gen.FSRequestEventOperationRead},
		{"attr", gen.FSRequestEventOperationRead},
		{"lookup", gen.FSRequestEventOperationRead},
		{"open", gen.FSRequestEventOperationRead},
		{"write", gen.FSRequestEventOperationWrite},
		{"open-write", gen.FSRequestEventOperationWrite},
		{"create", gen.FSRequestEventOperationWrite},
		{"mkdir", gen.FSRequestEventOperationWrite},
		{"truncate", gen.FSRequestEventOperationWrite},
		{"remove", gen.FSRequestEventOperationDelete}, // unlink and the source half of a rename
		{"rename", ""}, // renames are decomposed into write+remove, never emitted directly
		{"bogus", ""},
	}
	for _, tc := range cases {
		if got := fuseOpKind(tc.op); got != tc.want {
			t.Errorf("fuseOpKind(%q) = %q, want %q", tc.op, got, tc.want)
		}
	}
}

// TestSyncTranslator verifies the fs-sync audit stream maps onto a
// correlated system.fs-sync-request / system.fs-sync-response pair, and
// that the response carries the request's SSE id plus any backend error.
func TestSyncTranslator(t *testing.T) {
	broker := events.New(0, 0)
	tr := &syncTranslator{broker: broker, mount: "/workspace", backend: gen.BackendGcs, corr: newCorrelator()}

	tr.handle(map[string]any{
		"type": "fs-sync", "phase": "request", "request_id": "7",
		"op": "put", "path": "/workspace/report.md", "backend": "gcs",
	})
	tr.handle(map[string]any{
		"type": "fs-sync", "phase": "response", "request_id": "7",
		"duration_ms": float64(214), "err": "boom",
	})

	replay, _, cancel := broker.Subscribe(0)
	defer cancel()
	if len(replay) != 2 {
		t.Fatalf("got %d events, want 2", len(replay))
	}

	req, err := replay[0].Event.AsSystemFsSyncRequestEvent()
	if err != nil {
		t.Fatalf("event 0 not a fs-sync-request: %v", err)
	}
	if req.Backend != gen.BackendGcs || req.Operation != gen.SystemFsSyncRequestEventOperationPut || req.Path != "/workspace/report.md" {
		t.Errorf("request event = %+v", req)
	}

	resp, err := replay[1].Event.AsSystemFsSyncResponseEvent()
	if err != nil {
		t.Fatalf("event 1 not a fs-sync-response: %v", err)
	}
	if resp.RequestId != int(replay[0].ID) {
		t.Errorf("response request_id = %d, want %d", resp.RequestId, replay[0].ID)
	}
	if resp.DurationMs != 214 {
		t.Errorf("response duration_ms = %d, want 214", resp.DurationMs)
	}
	if resp.Error == nil || *resp.Error != "boom" {
		t.Errorf("response error = %v, want \"boom\"", resp.Error)
	}
}

// TestSyncRequestFactoryUnknownOp drops audit events whose op isn't a
// known Store operation, so a future backend method can't leak an
// unmapped value onto the stream.
func TestSyncRequestFactoryUnknownOp(t *testing.T) {
	if f := syncRequestFactory(map[string]any{"op": "frobnicate"}, gen.BackendGcs); f != nil {
		t.Errorf("expected nil factory for unknown op, got non-nil")
	}
}
