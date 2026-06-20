package main

import (
	"context"
	"errors"
	"testing"
	"time"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
)

func strptr(s string) *string { return &s }

func TestSupervisorRegisterAndResolve(t *testing.T) {
	sup := newSupervisor(false)
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
	sup := newSupervisor(false)
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
	// A non-prewarmed pod (claims==nil) already hosts its sandbox; a new key has
	// no free slot.
	sup := newSupervisor(false)
	sup.bootComplete()
	sup.register(handlers.NewSandbox("default", 0), "img:1", func() {})

	_, err := sup.Create(context.Background(), "other", gen.SandboxConfig{Image: strptr("img:1")})
	if !errors.Is(err, handlers.ErrPodOccupied) {
		t.Fatalf("Create(new key, non-prewarm) err = %v; want ErrPodOccupied", err)
	}
}

func TestSupervisorCreateImageMismatch(t *testing.T) {
	sup := newSupervisor(false)
	sup.bootComplete()
	sup.register(handlers.NewSandbox("default", 0), "img:1", func() {})

	_, err := sup.Create(context.Background(), "other", gen.SandboxConfig{Image: strptr("img:2")})
	if !errors.Is(err, handlers.ErrImageMismatch) {
		t.Fatalf("Create(mismatched image) err = %v; want ErrImageMismatch", err)
	}
}

func TestSupervisorCreatePrewarmClaim(t *testing.T) {
	// A prewarmed pod hands the claim to a consumer (here standing in for main),
	// which registers the sandbox under the claimed key and signals completion.
	sup := newSupervisor(true)
	sup.bootComplete()

	go func() {
		cl := <-sup.claims
		sb := handlers.NewSandbox(cl.key, 0)
		sup.register(sb, "img:1", func() {})
		cl.done <- nil
	}()

	sb, err := sup.Create(context.Background(), "agent-1", gen.SandboxConfig{})
	if err != nil {
		t.Fatalf("Create(prewarm claim) err = %v; want nil", err)
	}
	if sb == nil || sb.Key() != "agent-1" {
		t.Fatalf("Create(prewarm claim) sandbox = %v; want key agent-1", sb)
	}
	// register closed the claim window: a second create finds no free slot.
	if _, err := sup.Create(context.Background(), "agent-2", gen.SandboxConfig{}); !errors.Is(err, handlers.ErrPodOccupied) {
		t.Fatalf("second Create err = %v; want ErrPodOccupied", err)
	}
}

func TestSupervisorCreateClaimCancel(t *testing.T) {
	// With no consumer, Create blocks on the claim handoff until ctx is cancelled.
	sup := newSupervisor(true)
	sup.bootComplete()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := sup.Create(ctx, "agent-1", gen.SandboxConfig{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Create(no consumer) err = %v; want DeadlineExceeded", err)
	}
}

func TestSupervisorDelete(t *testing.T) {
	sup := newSupervisor(false)
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
