package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
)

func writeInitialConfig(t *testing.T, path string, cfg gen.SandboxConfig) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestConfigStore_Path(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewConfigStore(path)
	if s.Path() != path {
		t.Errorf("Path() = %q, want %q", s.Path(), path)
	}
}

func TestConfigStore_Get(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	fs := localFS("/workspace", &acls)
	want := gen.SandboxConfig{Fs: []gen.FileSystem{fs}}
	writeInitialConfig(t, path, want)

	s := NewConfigStore(path)
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Fs) != 1 {
		t.Errorf("got %d filesystems, want 1", len(got.Fs))
	}
}

func TestConfigStore_GetMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")
	s := NewConfigStore(path)
	if _, err := s.Get(); err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestConfigStore_Apply_WritesConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	initial := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}}
	writeInitialConfig(t, path, initial)

	s := NewConfigStore(path)
	acls2 := []gen.ACLRule{{Path: "/data/**", Access: gen.ACLRuleAccessRw}}
	desired := gen.SandboxConfig{Fs: []gen.FileSystem{
		localFS("/workspace", &acls),
		localFS("/data", &acls2),
	}}
	_, err := s.Apply(desired)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get after Apply: %v", err)
	}
	if len(got.Fs) != 2 {
		t.Errorf("after Apply: got %d filesystems, want 2", len(got.Fs))
	}
}

func TestConfigStore_Apply_ReturnsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	initial := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}}
	writeInitialConfig(t, path, initial)

	s := NewConfigStore(path)
	acls2 := []gen.ACLRule{{Path: "/data/**", Access: gen.ACLRuleAccessRw}}
	desired := gen.SandboxConfig{Fs: []gen.FileSystem{
		localFS("/workspace", &acls),
		localFS("/data", &acls2),
	}}
	ch, err := s.Apply(desired)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ch.Fs == nil || ch.Fs.Added == nil || len(*ch.Fs.Added) != 1 {
		t.Errorf("want 1 added fs, got %+v", ch.Fs)
	}
	if FSBase((*ch.Fs.Added)[0]).Mount != "/data" {
		t.Errorf("added mount = %q, want /data", FSBase((*ch.Fs.Added)[0]).Mount)
	}
}

func TestConfigStore_Apply_NoDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	cfg := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}}
	writeInitialConfig(t, path, cfg)

	s := NewConfigStore(path)
	ch, err := s.Apply(cfg)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ch.Fs != nil || ch.Egress != nil {
		t.Errorf("want empty diff for identical config, got %+v", ch)
	}
}

func TestConfigStore_Apply_Concurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	writeInitialConfig(t, path, gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}})

	s := NewConfigStore(path)
	s.mu.Lock() // simulate an Apply already holding the lock
	_, err := s.Apply(gen.SandboxConfig{})
	s.mu.Unlock()

	if !errors.Is(err, handlers.ErrApplyInProgress) {
		t.Errorf("want ErrApplyInProgress, got %v", err)
	}
}

func TestConfigStore_Apply_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	writeInitialConfig(t, path, gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}})

	s := NewConfigStore(path)
	acls2 := []gen.ACLRule{{Path: "/new/**", Access: gen.ACLRuleAccessRo}}
	if _, err := s.Apply(gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/new", &acls2)}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// No temp files should remain.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("unexpected file left in dir: %s", e.Name())
		}
	}
}
