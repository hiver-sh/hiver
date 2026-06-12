package remotefs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hiver-sh/hiver/internal/remotefs"
)

// fakeFSHost is a minimal in-memory implementation of the external file
// system HTTP contract (api/external_file_system.yaml). It backs the
// External client tests so we exercise the real request/response wiring
// without a live host.
type fakeFSHost struct {
	files map[string][]byte
}

func newFakeFSHost() *fakeFSHost { return &fakeFSHost{files: map[string][]byte{}} }

func (h *fakeFSHost) writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *fakeFSHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/list":
		var paths []string
		for p := range h.files {
			paths = append(paths, p)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"paths": paths})

	case r.Method == http.MethodGet && r.URL.Path == "/v1/stat":
		body, ok := h.files[q.Get("path")]
		if !ok {
			h.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":   q.Get("path"),
			"size":   len(body),
			"mtime":  time.Time{},
			"is_dir": false,
		})

	case r.Method == http.MethodGet && r.URL.Path == "/v1/file":
		body, ok := h.files[q.Get("path")]
		if !ok {
			h.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		_, _ = w.Write(body)

	case r.Method == http.MethodPut && r.URL.Path == "/v1/file":
		body, _ := io.ReadAll(r.Body)
		h.files[q.Get("path")] = body
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodDelete && r.URL.Path == "/v1/file":
		delete(h.files, q.Get("path"))
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && r.URL.Path == "/v1/move":
		var body struct{ Src, Dst string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		content, ok := h.files[body.Src]
		if !ok {
			h.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		h.files[body.Dst] = content
		delete(h.files, body.Src)
		w.WriteHeader(http.StatusNoContent)

	default:
		h.writeErr(w, http.StatusNotFound, "unknown endpoint")
	}
}

// TestExternalRoundTrip drives the External client against an in-memory
// host implementing the external file system contract: put, list, stat,
// get, move, delete, plus the ErrNotExist mapping for 404s.
func TestExternalRoundTrip(t *testing.T) {
	srv := httptest.NewServer(newFakeFSHost())
	defer srv.Close()

	s, err := remotefs.NewExternal(context.Background(), remotefs.ExternalConfig{Host: srv.URL}, 0, nil)
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	ctx := context.Background()

	if err := s.Put(ctx, "/foo.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	paths, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/foo.txt" {
		t.Errorf("List: got %v, want [/foo.txt]", paths)
	}

	fi, err := s.Stat(ctx, "/foo.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Path != "/foo.txt" || fi.Size != 5 {
		t.Errorf("Stat: got %+v, want path=/foo.txt size=5", fi)
	}

	rc, err := s.Get(ctx, "/foo.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "hello" {
		t.Errorf("Get content: got %q, want %q", body, "hello")
	}

	if _, err := s.Get(ctx, "/missing.txt"); !errors.Is(err, remotefs.ErrNotExist) {
		t.Errorf("Get missing: got %v, want ErrNotExist", err)
	}
	if _, err := s.Stat(ctx, "/missing.txt"); !errors.Is(err, remotefs.ErrNotExist) {
		t.Errorf("Stat missing: got %v, want ErrNotExist", err)
	}

	if err := s.Move(ctx, "/foo.txt", "/renamed.txt"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := s.Get(ctx, "/foo.txt"); !errors.Is(err, remotefs.ErrNotExist) {
		t.Errorf("after Move: source still exists, got err=%v", err)
	}

	if err := s.Delete(ctx, "/renamed.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "/renamed.txt"); err != nil {
		t.Errorf("idempotent Delete: %v", err)
	}
}

func TestNewExternalRejectsBadHost(t *testing.T) {
	if _, err := remotefs.NewExternal(context.Background(), remotefs.ExternalConfig{Host: ""}, 0, nil); err == nil {
		t.Error("empty host: want error, got nil")
	}
	if _, err := remotefs.NewExternal(context.Background(), remotefs.ExternalConfig{Host: "fs.internal:8080"}, 0, nil); err == nil {
		t.Error("scheme-less host: want error, got nil")
	}
}
