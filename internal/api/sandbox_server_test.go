package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
)

// fakeSupervisor is a minimal handlers.Supervisor for routing tests.
type fakeSupervisor struct {
	sandboxes map[string]*handlers.Sandbox
}

func (f *fakeSupervisor) Sandbox(key string) (*handlers.Sandbox, bool) {
	sb, ok := f.sandboxes[key]
	return sb, ok
}

func (f *fakeSupervisor) Create(context.Context, string, gen.SandboxConfig) (*handlers.Sandbox, error) {
	return nil, handlers.ErrPodOccupied
}

func (f *fakeSupervisor) Delete(_ context.Context, key string) error {
	if _, ok := f.sandboxes[key]; !ok {
		return handlers.ErrSandboxNotFound
	}
	return nil
}

func (f *fakeSupervisor) List() []*handlers.Sandbox {
	out := make([]*handlers.Sandbox, 0, len(f.sandboxes))
	for _, sb := range f.sandboxes {
		out = append(out, sb)
	}
	return out
}

func (f *fakeSupervisor) SubscribeLifecycle() (<-chan gen.PodEvent, func()) {
	ch := make(chan gen.PodEvent)
	return ch, func() {}
}

// readySandbox builds a sandbox wired with a config store and flipped ready, so
// keyed routes that only need the store (e.g. GET config) serve through it.
func readySandbox(key string) *handlers.Sandbox {
	sb := handlers.NewSandbox(key, 0)
	sb.SetStore(NewConfigStore(gen.SandboxConfig{}))
	sb.NotifyReady()
	return sb
}

func TestSandboxServerKeyedRouting(t *testing.T) {
	sup := &fakeSupervisor{sandboxes: map[string]*handlers.Sandbox{
		"default": readySandbox("default"),
	}}
	h := NewSandboxServer("0", sup).Handler

	cases := []struct {
		name, method, path string
		want               int
	}{
		{"keyed ping ready", http.MethodGet, "/v1/default/ping", http.StatusOK},
		{"keyed ping unknown key 404", http.MethodGet, "/v1/missing/ping", http.StatusNotFound},
		{"old pod ping gone", http.MethodGet, "/v1/ping", http.StatusNotFound},
		{"list sandboxes", http.MethodGet, "/v1", http.StatusOK},
		{"keyed config resolves", http.MethodGet, "/v1/default/config", http.StatusOK},
		{"unknown key 404", http.MethodGet, "/v1/missing/config", http.StatusNotFound},
		{"old unkeyed route gone", http.MethodGet, "/v1/config", http.StatusNotFound},
		{"create new key on occupied pod", http.MethodPost, "/v1/other", http.StatusConflict},
		{"delete unknown key 404", http.MethodDelete, "/v1/missing", http.StatusNotFound},
		{"delete known key 204", http.MethodDelete, "/v1/default", http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.method == http.MethodPost {
				body = strings.NewReader(`{"image":"img:1"}`)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("%s %s = %d; want %d (body: %s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
			}
		})
	}
}
