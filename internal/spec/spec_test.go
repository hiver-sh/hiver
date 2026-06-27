package spec_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiver-sh/hiver/internal/spec"
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
		"env": {"FOO": "bar"},
		"fs":     [{"backend": "local", "mount": "/work",
		            "acls": [{"path": "/work", "access": "rw"}]}],
		"egress": [{"access": "allow", "host": "api.github.com", "methods": ["GET"], "paths": ["/repos/*"]}]
	}`)
	s, err := spec.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Env) != 1 {
		t.Errorf("env: %+v", s.Env)
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
	if got := s.Egress; len(got) != 1 || got[0].Access != "allow" || got[0].Host != "api.github.com" || len(got[0].Methods) != 1 || got[0].Methods[0] != "GET" {
		t.Errorf("egress: %+v", got)
	}
}

func TestLoadEntrypoint(t *testing.T) {
	// entrypoint accepts both a JSON array and a single string (split on
	// whitespace), normalizing both to the same argv slice.
	cases := []struct {
		name string
		json string
		want []string
	}{
		{"array", `{"entrypoint": ["tail", "-f", "/dev/null"], "fs": []}`, []string{"tail", "-f", "/dev/null"}},
		{"string", `{"entrypoint": "tail -f /dev/null", "fs": []}`, []string{"tail", "-f", "/dev/null"}},
		{"string extra spaces", `{"entrypoint": "  sleep   3600 ", "fs": []}`, []string{"sleep", "3600"}},
		{"omitted", `{"fs": []}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := spec.Load(writeSpec(t, tc.json))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(s.Entrypoint) != len(tc.want) {
				t.Fatalf("entrypoint: got %+v, want %+v", s.Entrypoint, tc.want)
			}
			for i := range tc.want {
				if s.Entrypoint[i] != tc.want[i] {
					t.Errorf("entrypoint[%d]: got %q, want %q", i, s.Entrypoint[i], tc.want[i])
				}
			}
		})
	}
}

func TestLoadNoFS(t *testing.T) {
	// fs is optional: a prewarm sandbox boots with only an image and
	// receives its mounts later via PUT /v1/config, so an absent or empty fs
	// must validate.
	for _, body := range []string{`{}`, `{"fs":[]}`} {
		p := writeSpec(t, body)
		s, err := spec.Load(p)
		if err != nil {
			t.Fatalf("Load(%s): %v", body, err)
		}
		if len(s.FS) != 0 {
			t.Errorf("fs: want empty, got %+v", s.FS)
		}
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

func TestDefaultACL(t *testing.T) {
	p := writeSpec(t, `{"fs":[{"backend":"local","mount":"/workspace"}]}`)
	s, err := spec.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	acls := s.FS[0].ACLs
	if len(acls) != 1 || acls[0].Path != "/workspace/**" || string(acls[0].Access) != "rw" {
		t.Errorf("default ACL: got %+v, want [{/workspace/** rw}]", acls)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
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

func TestOverrideHostValidation(t *testing.T) {
	mk := func(rule string) string {
		return `{
			"fs": [{"backend": "local", "mount": "/work"}],
			"egress": [` + rule + `]
		}`
	}
	cases := []struct {
		name    string
		rule    string
		wantErr string // empty means must load
	}{
		{
			"valid host with port",
			`{"access": "allow", "host": "api.example.com", "override": {"host": "stub.internal:17080"}}`,
			"",
		},
		{
			"valid host without port",
			`{"access": "allow", "host": "api.example.com", "override": {"host": "stub.internal"}}`,
			"",
		},
		{
			"scheme rejected",
			`{"access": "allow", "host": "api.example.com", "override": {"host": "http://stub.internal"}}`,
			"override.host",
		},
		{
			"path rejected",
			`{"access": "allow", "host": "api.example.com", "override": {"host": "stub.internal/v1"}}`,
			"override.host",
		},
		{
			"wildcard rejected",
			`{"access": "allow", "host": "api.example.com", "override": {"host": "*.internal"}}`,
			"override.host",
		},
		{
			"port out of range",
			`{"access": "allow", "host": "api.example.com", "override": {"host": "stub.internal:99999"}}`,
			"override.host",
		},
		{
			"deny rule rejected",
			`{"access": "deny", "host": "api.example.com", "override": {"host": "stub.internal:8080"}}`,
			"override.host",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := spec.Load(writeSpec(t, mk(c.rule)))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Load: got %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

func TestSnapshotMountValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string // empty means must load
	}{
		{
			"mount references a declared fs",
			`{"fs": [{"backend": "external", "mount": "/snapshots", "host": "https://h", "internal": true}],
			  "snapshot": {"files": {"key": "k", "mount": "/snapshots"}}}`,
			"",
		},
		{
			"omitted mount is fine",
			`{"fs": [{"backend": "local", "mount": "/work"}],
			  "snapshot": {"files": {"key": "k"}}}`,
			"",
		},
		{
			"vm-only snapshot is fine",
			`{"fs": [{"backend": "local", "mount": "/work"}],
			  "snapshot": {"vm": {"key": "k"}}}`,
			"",
		},
		{
			"relative mount rejected",
			`{"fs": [{"backend": "local", "mount": "/work"}],
			  "snapshot": {"files": {"key": "k", "mount": "snapshots"}}}`,
			"snapshot.files.mount",
		},
		{
			"mount with no matching fs rejected",
			`{"fs": [{"backend": "local", "mount": "/work"}],
			  "snapshot": {"files": {"key": "k", "mount": "/snapshots"}}}`,
			"does not match any fs",
		},
		{
			"files without key rejected",
			`{"fs": [{"backend": "local", "mount": "/work"}],
			  "snapshot": {"files": {"mount": "/work"}}}`,
			"snapshot.files.key",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := spec.Load(writeSpec(t, c.body))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Load: got %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

func TestOverridePrefixPathValidation(t *testing.T) {
	mk := func(rule string) string {
		return `{
			"fs": [{"backend": "local", "mount": "/work"}],
			"egress": [` + rule + `]
		}`
	}
	cases := []struct {
		name    string
		rule    string
		wantErr string // empty means must load
	}{
		{
			"valid prefix",
			`{"access": "allow", "host": "api.example.com", "override": {"prefix_path": "/mock"}}`,
			"",
		},
		{
			"valid with trailing slash",
			`{"access": "allow", "host": "api.example.com", "override": {"prefix_path": "/mock/"}}`,
			"",
		},
		{
			"relative rejected",
			`{"access": "allow", "host": "api.example.com", "override": {"prefix_path": "mock"}}`,
			"override.prefix_path",
		},
		{
			"wildcard rejected",
			`{"access": "allow", "host": "api.example.com", "override": {"prefix_path": "/mock/*"}}`,
			"override.prefix_path",
		},
		{
			"query char rejected",
			`{"access": "allow", "host": "api.example.com", "override": {"prefix_path": "/mock?x=1"}}`,
			"override.prefix_path",
		},
		{
			"deny rule rejected",
			`{"access": "deny", "host": "api.example.com", "override": {"prefix_path": "/mock"}}`,
			"override.prefix_path",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := spec.Load(writeSpec(t, mk(c.rule)))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Load: got %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}
