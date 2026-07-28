package fusefs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/remotefs"
)

// decodeSyncEvents parses the JSON-line audit stream into SyncAuditEvents.
func decodeSyncEvents(t *testing.T, r *bytes.Buffer) []fusefs.SyncAuditEvent {
	t.Helper()
	var out []fusefs.SyncAuditEvent
	dec := json.NewDecoder(strings.NewReader(r.String()))
	for dec.More() {
		var e fusefs.SyncAuditEvent
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("decode audit line: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// TestAuditedStoreEmitsPairs checks a Put emits a request/response pair
// with the host mount prepended to the store-relative path, the backend
// name attached, and a shared request id.
func TestAuditedStoreEmitsPairs(t *testing.T) {
	inner, err := remotefs.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	buf := &bytes.Buffer{}
	store := fusefs.NewAuditedStore(inner, "gcs", "/run/sandboxd/k/mnt", buf)

	if err := store.Put(context.Background(), "/report.md", strings.NewReader("hi")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	evs := decodeSyncEvents(t, buf)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (request+response)", len(evs))
	}
	req, resp := evs[0], evs[1]
	if req.Type != fusefs.SyncAuditType || req.Phase != "request" {
		t.Errorf("event 0 = %+v, want fs-sync request", req)
	}
	if req.Op != "put" || req.Backend != "gcs" || req.Path != "/run/sandboxd/k/mnt/report.md" {
		t.Errorf("request event = %+v", req)
	}
	if resp.Phase != "response" || resp.RequestID != req.RequestID {
		t.Errorf("response event = %+v (req id %s)", resp, req.RequestID)
	}
	if resp.Err != "" {
		t.Errorf("unexpected response err %q", resp.Err)
	}
}

// TestAuditedStoreSuppressesNotExist confirms a Stat miss (ErrNotExist)
// still emits a response but reports no error — a missing file is normal
// control flow, not a backend failure.
func TestAuditedStoreSuppressesNotExist(t *testing.T) {
	inner, err := remotefs.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	buf := &bytes.Buffer{}
	store := fusefs.NewAuditedStore(inner, "s3", "/workspace", buf)

	if _, err := store.Stat(context.Background(), "/missing"); err != remotefs.ErrNotExist {
		t.Fatalf("Stat err = %v, want ErrNotExist", err)
	}

	evs := decodeSyncEvents(t, buf)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[1].Phase != "response" || evs[1].Err != "" {
		t.Errorf("response = %+v, want no err for ErrNotExist", evs[1])
	}
}

// TestNewAuditedStoreNilPassthrough keeps local-only mounts (nil store)
// free of a wrapper — there is no external backend to audit.
func TestNewAuditedStoreNilPassthrough(t *testing.T) {
	if got := fusefs.NewAuditedStore(nil, "local", "/workspace", &bytes.Buffer{}); got != nil {
		t.Errorf("NewAuditedStore(nil) = %v, want nil", got)
	}
}
