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

// newTestSandbox returns a Sandbox whose API URL points at srv. The routing
// segment is the sandbox id, so id is what flows into /sandbox/<id>.
func newTestSandbox(srv *httptest.Server, id string) *Sandbox {
	return newSandbox(SandboxRef{ID: id, Key: "test-key"}, srv.URL, &http.Client{})
}

func writeSSEFrame(w http.ResponseWriter, v interface{}) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()
}

func TestSandbox_ProxyURL(t *testing.T) {
	s := &Sandbox{apiURL: "http://gw/sandbox/k"}
	if got := s.ProxyURL(8080); got != "http://gw/sandbox/k/v1/proxy/8080" {
		t.Errorf("got %q", got)
	}
}


func TestSandbox_Ping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		assertPath(t, r, "/sandbox/k/v1/ping")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := newTestSandbox(srv, "k").Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSandbox_Ping_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, APIError{Message: "not ready"})
	}))
	defer srv.Close()

	err := newTestSandbox(srv, "k").Ping(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSandbox_GetConfig(t *testing.T) {
	cfg := SandboxConfig{Image: "my-image:latest", CPU: 2}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		assertPath(t, r, "/sandbox/k/v1/config")
		writeJSON(w, http.StatusOK, cfg)
	}))
	defer srv.Close()

	got, err := newTestSandbox(srv, "k").GetConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != cfg.Image || got.CPU != cfg.CPU {
		t.Errorf("got %+v", got)
	}
}

func TestSandbox_ApplyConfig(t *testing.T) {
	result := ApplyResult{Applied: true, Config: SandboxConfig{Image: "new:v2"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPut)
		assertPath(t, r, "/sandbox/k/v1/config")
		writeJSON(w, http.StatusOK, result)
	}))
	defer srv.Close()

	got, err := newTestSandbox(srv, "k").ApplyConfig(context.Background(), SandboxConfig{Image: "new:v2"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Applied || got.Config.Image != "new:v2" {
		t.Errorf("got %+v", got)
	}
}

func TestSandbox_GetPorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPath(t, r, "/sandbox/k/v1/ports")
		writeJSON(w, http.StatusOK, []int{8080, 9000})
	}))
	defer srv.Close()

	ports, err := newTestSandbox(srv, "k").GetPorts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || ports[0] != 8080 || ports[1] != 9000 {
		t.Errorf("got %v", ports)
	}
}

func TestSandbox_Exec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		assertPath(t, r, "/sandbox/k/v1/exec")

		var req ExecRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Command != "echo hi" {
			t.Errorf("unexpected command: %q", req.Command)
		}
		writeJSON(w, http.StatusOK, ExecResult{Stdout: "hi\n", ExitCode: 0})
	}))
	defer srv.Close()

	result, err := newTestSandbox(srv, "k").Exec(context.Background(), ExecRequest{Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "hi\n" || result.ExitCode != 0 {
		t.Errorf("got %+v", result)
	}
}

func TestSandbox_ExecStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stdin") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSEFrame(w, map[string]interface{}{"type": "stdout", "text": "hello\n"})
		writeSSEFrame(w, map[string]interface{}{"type": "stderr", "text": "warn\n"})
		writeSSEFrame(w, map[string]interface{}{"type": "exit", "code": 42})
	}))
	defer srv.Close()

	proc, err := newTestSandbox(srv, "k").ExecStream(context.Background(), ExecStreamRequest{Command: "run"})
	if err != nil {
		t.Fatal(err)
	}

	out1 := <-proc.Output
	if out1.Stdout != "hello\n" {
		t.Errorf("stdout: got %q", out1.Stdout)
	}
	out2 := <-proc.Output
	if out2.Stderr != "warn\n" {
		t.Errorf("stderr: got %q", out2.Stderr)
	}

	code, err := proc.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if code != 42 {
		t.Errorf("exit code: got %d, want 42", code)
	}
}

func TestSandbox_ListDirectory(t *testing.T) {
	entries := []DirEntry{
		{Name: "file.txt", Path: "/workspace/file.txt", IsDir: false, Size: 42},
		{Name: "subdir", Path: "/workspace/subdir", IsDir: true, Size: 0},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPath(t, r, "/sandbox/k/v1/directories")
		if r.URL.Query().Get("path") != "/workspace" {
			t.Errorf("unexpected path param: %q", r.URL.Query().Get("path"))
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"entries": entries})
	}))
	defer srv.Close()

	got, err := newTestSandbox(srv, "k").ListDirectory(context.Background(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "file.txt" || !got[1].IsDir {
		t.Errorf("got %+v", got)
	}
}

func TestSandbox_ReadFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPath(t, r, "/sandbox/k/v1/file")
		if r.URL.Query().Get("path") != "/workspace/data.csv" {
			t.Errorf("unexpected path param: %q", r.URL.Query().Get("path"))
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("col1,col2\n1,2\n"))
	}))
	defer srv.Close()

	data, err := newTestSandbox(srv, "k").ReadFile(context.Background(), "/workspace/data.csv")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "col1,col2\n1,2\n" {
		t.Errorf("got %q", data)
	}
}

func TestSandbox_WriteFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		assertPath(t, r, "/sandbox/k/v1/file")

		r.ParseMultipartForm(1 << 20)
		if dst := r.FormValue("destination"); dst != "/workspace" {
			t.Errorf("destination: got %q", dst)
		}
		writeJSON(w, http.StatusOK, UploadResult{Path: "/workspace/hello.txt", Bytes: 5})
	}))
	defer srv.Close()

	result, err := newTestSandbox(srv, "k").WriteFile(context.Background(), "/workspace", "hello.txt", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "/workspace/hello.txt" || result.Bytes != 5 {
		t.Errorf("got %+v", result)
	}
}

func TestSandbox_WatchEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPath(t, r, "/sandbox/k/v1/events")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSEFrame(w, SandboxEvent{ID: 1, Type: "stdio", Stdout: "hello\n"})
		writeSSEFrame(w, SandboxEvent{ID: 2, Type: "stdio", Stdout: "world\n"})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, _ := newTestSandbox(srv, "k").WatchEvents(ctx, -1)

	e1 := <-events
	if e1.Type != "stdio" || e1.Stdout != "hello\n" {
		t.Errorf("event 1: %+v", e1)
	}
	e2 := <-events
	if e2.Type != "stdio" || e2.Stdout != "world\n" {
		t.Errorf("event 2: %+v", e2)
	}
}

func TestSandbox_WatchEvents_LastEventID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("lastEventId"); got != "5" {
			t.Errorf("lastEventId: got %q, want %q", got, "5")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately — we only care that the query param was sent

	newTestSandbox(srv, "k").WatchEvents(ctx, 5)
}
