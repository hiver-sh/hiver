package api

import (
	"errors"
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
)

func TestConfigStore_Get(t *testing.T) {
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	want := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}}

	s := NewConfigStore(want)
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Fs) != 1 {
		t.Errorf("got %d filesystems, want 1", len(got.Fs))
	}
}

func TestConfigStore_Apply_UpdatesConfig(t *testing.T) {
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	s := NewConfigStore(gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}})

	acls2 := []gen.ACLRule{{Path: "/data/**", Access: gen.ACLRuleAccessRw}}
	desired := gen.SandboxConfig{Fs: []gen.FileSystem{
		localFS("/workspace", &acls),
		localFS("/data", &acls2),
	}}
	if _, err := s.Apply(desired); err != nil {
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
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	s := NewConfigStore(gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}})

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
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	cfg := gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}}
	s := NewConfigStore(cfg)

	ch, err := s.Apply(cfg)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ch.Fs != nil || ch.Egress != nil {
		t.Errorf("want empty diff for identical config, got %+v", ch)
	}
}

func TestConfigStore_Apply_Concurrent(t *testing.T) {
	acls := []gen.ACLRule{{Path: "/workspace/**", Access: gen.ACLRuleAccessRw}}
	s := NewConfigStore(gen.SandboxConfig{Fs: []gen.FileSystem{localFS("/workspace", &acls)}})

	s.applyMu.Lock() // simulate an Apply already in flight
	_, err := s.Apply(gen.SandboxConfig{})
	s.applyMu.Unlock()

	if !errors.Is(err, handlers.ErrApplyInProgress) {
		t.Errorf("want ErrApplyInProgress, got %v", err)
	}
}
