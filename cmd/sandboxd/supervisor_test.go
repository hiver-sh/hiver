package main

import (
	"context"
	"errors"
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
)

func strptr(s string) *string { return &s }

func TestSupervisorRegisterAndResolve(t *testing.T) {
	sup := newSupervisor()
	sup.bootComplete()
	sb := handlers.NewSandbox("default", 0)
	sup.register(sb, "img:1", func() {})

	got, ok := sup.Sandbox("default")
	if !ok || got != sb {
		t.Fatalf("Sandbox(default) = %v, %v; want the registered sandbox", got, ok)
	}
	if _, ok := sup.Sandbox("missing"); ok {
		t.Fatalf("Sandbox(missing) reported present")
	}
	if list := sup.List(); len(list) != 1 || list[0] != sb {
		t.Fatalf("List() = %v; want [the registered sandbox]", list)
	}
}

func TestSupervisorCreateIdempotent(t *testing.T) {
	sup := newSupervisor()
	sup.bootComplete()
	sb := handlers.NewSandbox("default", 0)
	sup.register(sb, "img:1", func() {})

	// Existing key returns its sandbox regardless of the supplied config.
	got, err := sup.Create(context.Background(), "default", gen.SandboxConfig{})
	if err != nil || got != sb {
		t.Fatalf("Create(existing) = %v, %v; want the registered sandbox, nil", got, err)
	}
}

func TestSupervisorCreateOccupied(t *testing.T) {
	// A non-pack pod already hosts its single sandbox; a new key has no free slot.
	sup := newSupervisor()
	sup.bootComplete()
	sup.register(handlers.NewSandbox("default", 0), "img:1", func() {})

	_, err := sup.Create(context.Background(), "other", gen.SandboxConfig{Image: strptr("img:1")})
	if !errors.Is(err, handlers.ErrPodOccupied) {
		t.Fatalf("Create(new key, non-pack) err = %v; want ErrPodOccupied", err)
	}
}

func TestSupervisorDelete(t *testing.T) {
	sup := newSupervisor()
	sup.bootComplete()
	cancelled := false
	sup.register(handlers.NewSandbox("default", 0), "img:1", func() { cancelled = true })

	if err := sup.Delete(context.Background(), "missing"); !errors.Is(err, handlers.ErrSandboxNotFound) {
		t.Fatalf("Delete(missing) err = %v; want ErrSandboxNotFound", err)
	}
	if err := sup.Delete(context.Background(), "default"); err != nil {
		t.Fatalf("Delete(default) err = %v; want nil", err)
	}
	if !cancelled {
		t.Fatalf("Delete(default) did not invoke the lifecycle cancel")
	}
}
