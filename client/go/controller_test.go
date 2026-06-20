package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient returns a Client pointed at srv with no readiness wait.
func newTestClient(srv *httptest.Server) *Client {
	return NewClient(srv.URL, WithTimeout(0))
}

func assertMethod(t *testing.T, r *http.Request, method string) {
	t.Helper()
	if r.Method != method {
		t.Errorf("method: got %s, want %s", r.Method, method)
	}
}

func assertPath(t *testing.T, r *http.Request, path string) {
	t.Helper()
	if r.URL.Path != path {
		t.Errorf("path: got %s, want %s", r.URL.Path, path)
	}
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func TestClient_GetOrCreateSandbox_InvalidKey(t *testing.T) {
	c := NewClient("http://localhost", WithTimeout(0))
	_, err := c.GetOrCreateSandbox(context.Background(), "bad key!", SandboxConfig{})
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
}

func TestClient_GetOrCreateSandbox_201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPut)
		assertPath(t, r, "/controller/v1/sandboxes/my-key")
		writeJSON(w, http.StatusCreated, SandboxRef{ID: "id-1", Key: "my-key"})
	}))
	defer srv.Close()

	sbx, err := newTestClient(srv).GetOrCreateSandbox(context.Background(), "my-key", SandboxConfig{
		FS: []FileSystem{{Mount: "/workspace", Backend: "local"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sbx.ID != "id-1" || sbx.Key != "my-key" {
		t.Errorf("got %+v", sbx)
	}
}

func TestClient_GetOrCreateSandbox_200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, SandboxRef{ID: "id-existing", Key: "my-key"})
	}))
	defer srv.Close()

	sbx, err := newTestClient(srv).GetOrCreateSandbox(context.Background(), "my-key", SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if sbx.ID != "id-existing" {
		t.Errorf("got id %q", sbx.ID)
	}
}

func TestClient_GetOrCreateSandbox_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, APIError{Message: "invalid config"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).GetOrCreateSandbox(context.Background(), "my-key", SandboxConfig{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClient_ListSandboxes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		assertPath(t, r, "/controller/v1/sandboxes")
		writeJSON(w, http.StatusOK, []SandboxRef{
			{ID: "id-1", Key: "key-1"},
			{ID: "id-2", Key: "key-2"},
		})
	}))
	defer srv.Close()

	sandboxes, err := newTestClient(srv).ListSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxes) != 2 {
		t.Fatalf("got %d sandboxes, want 2", len(sandboxes))
	}
	if sandboxes[0].Key != "key-1" || sandboxes[1].Key != "key-2" {
		t.Errorf("keys: %q %q", sandboxes[0].Key, sandboxes[1].Key)
	}
}

func TestClient_ListSandboxes_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []SandboxRef{})
	}))
	defer srv.Close()

	sandboxes, err := newTestClient(srv).ListSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxes) != 0 {
		t.Errorf("expected empty, got %d", len(sandboxes))
	}
}

func TestClient_WatchEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPath(t, r, "/controller/v1/sandboxes/events")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, e := range []SandboxLifecycleEvent{
			{ID: "id-1", Key: "key-1", Status: "start"},
			{ID: "id-2", Key: "key-2", Status: "stop"},
		} {
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, _ := newTestClient(srv).WatchEvents(ctx)

	e1 := <-events
	if e1.Key != "key-1" || e1.Status != "start" {
		t.Errorf("event 1: %+v", e1)
	}
	e2 := <-events
	if e2.Key != "key-2" || e2.Status != "stop" {
		t.Errorf("event 2: %+v", e2)
	}
}
