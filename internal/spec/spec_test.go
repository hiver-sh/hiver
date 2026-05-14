package spec_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandbox-platform/agent-sandbox/internal/spec"
)

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spec.json")
	if err := writeFile(p, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func writeFile(p, body string) error {
	return writeFileImpl(p, body)
}

func TestLoadValid(t *testing.T) {
	p := writeSpec(t, `{
		"agent":  {"env": ["FOO=bar"]},
		"fs":     [{"backend": "local", "mount": "/work",
		            "acls": [{"path": "/work", "access": "rw"}]}],
		"egress": {"allow": [{"host": "api.github.com", "methods": ["GET"], "paths": ["/repos/*"]}]}
	}`)
	s, err := spec.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Agent.Env) != 1 {
		t.Errorf("agent: %+v", s.Agent)
	}
	if len(s.FS) != 1 || s.FS[0].Mount != "/work" || len(s.FS[0].ACLs) != 1 {
		t.Errorf("fs: %+v", s.FS)
	}
	if !s.FS[0].Backend.Valid() {
		t.Errorf("fs[0].backend: not valid: %q", s.FS[0].Backend)
	}
	if got, want := s.FS[0].BackendPath(), "/work-backend"; got != want {
		t.Errorf("fs[0] BackendPath: got %q, want %q", got, want)
	}
	if got := s.Egress.Allow; len(got) != 1 || got[0].Host != "api.github.com" || len(got[0].Methods) != 1 || got[0].Methods[0] != "GET" {
		t.Errorf("egress: %+v", got)
	}
}

func TestLoadMultipleMounts(t *testing.T) {
	p := writeSpec(t, `{
		"fs": [
			{"backend":"local","mount":"/work","acls":[{"path":"/work","access":"rw"}]},
			{"backend":"local","mount":"/data","acls":[{"path":"/data","access":"ro"}]}
		]
	}`)
	s, err := spec.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.FS) != 2 {
		t.Fatalf("fs: want 2 entries, got %d", len(s.FS))
	}
	if s.FS[0].Slug() != "work" || s.FS[1].Slug() != "data" {
		t.Errorf("slugs: %q, %q", s.FS[0].Slug(), s.FS[1].Slug())
	}
}

func TestLoadMissingRequired(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no fs", `{}`, "fs is required"},
		{"empty fs", `{"fs":[]}`, "fs is required"},
		{"no backend", `{"fs":[{"mount":"/m"}]}`, "backend"},
		{"unknown backend", `{"fs":[{"backend":"s3","mount":"/m"}]}`, "backend"},
		{"no mount", `{"fs":[{"backend":"local"}]}`, "mount"},
		{"relative mount", `{"fs":[{"backend":"local","mount":"work"}]}`, "absolute path"},
		{"duplicate mount", `{"fs":[{"backend":"local","mount":"/m"},{"backend":"local","mount":"/m"}]}`, "overlaps"},
		{"prefix mount", `{"fs":[{"backend":"local","mount":"/m"},{"backend":"local","mount":"/m/sub"}]}`, "overlaps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeSpec(t, tc.body)
			_, err := spec.Load(p)
			if err == nil {
				t.Fatal("expected error; got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%q, want substring %q", err, tc.want)
			}
		})
	}
}
