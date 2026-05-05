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
		"fs":     {"backend": "local", "mount": "/work",
		           "acls": [{"path": "/work", "access": "rw"}]},
		"egress": {"allow": [{"host": "api.github.com", "methods": ["GET"], "paths": ["/repos/*"]}]},
		"audit_dir": "/audit"
	}`)
	s, err := spec.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Agent.Env) != 1 {
		t.Errorf("agent: %+v", s.Agent)
	}
	if s.FS.Mount != "/work" || len(s.FS.ACLs) != 1 {
		t.Errorf("fs: %+v", s.FS)
	}
	if got := s.FS.Backend.HostPath(); got != spec.LocalBackendPath {
		t.Errorf("fs.backend HostPath: got %q, want %q", got, spec.LocalBackendPath)
	}
	if got := s.Egress.Allow; len(got) != 1 || got[0].Host != "api.github.com" || len(got[0].Methods) != 1 || got[0].Methods[0] != "GET" {
		t.Errorf("egress: %+v", got)
	}
	if s.AuditDir != "/audit" {
		t.Errorf("audit_dir: %q", s.AuditDir)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no backend", `{"fs":{"mount":"/m"},"audit_dir":"/a"}`, "fs.backend"},
		{"unknown backend", `{"fs":{"backend":"s3","mount":"/m"},"audit_dir":"/a"}`, "fs.backend"},
		{"no mount", `{"fs":{"backend":"local"},"audit_dir":"/a"}`, "fs.mount"},
		{"no audit_dir", `{"fs":{"backend":"local","mount":"/m"}}`, "audit_dir"},
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
