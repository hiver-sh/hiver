package handlers

import (
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

func ptr[T any](v T) *T { return &v }

func iso(v string) *gen.SandboxConfigIsolation {
	x := gen.SandboxConfigIsolation(v)
	return &x
}

func TestFreezeImmutable(t *testing.T) {
	// image and isolation are frozen whenever already set: an apply can't change
	// them, only set them when unset.
	t.Run("image/isolation frozen once set", func(t *testing.T) {
		current := gen.SandboxConfig{Image: ptr("base"), Isolation: iso("microvm")}
		desired := gen.SandboxConfig{Image: ptr("other"), Isolation: iso("container")}
		got := freezeImmutable(current, desired, false)
		if *got.Image != "base" {
			t.Errorf("image = %q, want base (frozen)", *got.Image)
		}
		if *got.Isolation != "microvm" {
			t.Errorf("isolation = %q, want microvm (frozen)", *got.Isolation)
		}
	})

	t.Run("image/isolation settable when unset", func(t *testing.T) {
		current := gen.SandboxConfig{}
		desired := gen.SandboxConfig{Image: ptr("base"), Isolation: iso("microvm")}
		got := freezeImmutable(current, desired, false)
		if got.Image == nil || *got.Image != "base" {
			t.Errorf("image = %v, want base (settable)", got.Image)
		}
		if got.Isolation == nil || *got.Isolation != "microvm" {
			t.Errorf("isolation = %v, want microvm (settable)", got.Isolation)
		}
	})

	// cpu/memory/entrypoint/cwd/tty/env are settable while prewarm (not started)
	// and frozen afterward.
	t.Run("boot-time fields settable before start", func(t *testing.T) {
		current := gen.SandboxConfig{Cpu: ptr(1), Env: &map[string]string{"A": "1"}}
		desired := gen.SandboxConfig{Cpu: ptr(4), Env: &map[string]string{"A": "2"}}
		got := freezeImmutable(current, desired, false)
		if *got.Cpu != 4 {
			t.Errorf("cpu = %d, want 4 (settable before start)", *got.Cpu)
		}
		if (*got.Env)["A"] != "2" {
			t.Errorf("env A = %q, want 2 (settable before start)", (*got.Env)["A"])
		}
	})

	t.Run("boot-time fields frozen after start", func(t *testing.T) {
		current := gen.SandboxConfig{Cpu: ptr(1), Memory: ptr(512), Entrypoint: ptr("sh"), Tty: ptr(true), Env: &map[string]string{"A": "1"}}
		desired := gen.SandboxConfig{Cpu: ptr(4), Memory: ptr(1024), Entrypoint: ptr("bash"), Tty: ptr(false), Env: &map[string]string{"A": "2"}}
		got := freezeImmutable(current, desired, true)
		if *got.Cpu != 1 || *got.Memory != 512 || *got.Entrypoint != "sh" || *got.Tty != true || (*got.Env)["A"] != "1" {
			t.Errorf("boot-time fields not frozen after start: %+v", got)
		}
	})

	// fs/egress/ttl/snapshot are reconciled at runtime, never frozen.
	t.Run("runtime fields pass through even after start", func(t *testing.T) {
		current := gen.SandboxConfig{Ttl: ptr(60)}
		desired := gen.SandboxConfig{Ttl: ptr(120), Fs: []gen.FileSystem{{}}}
		got := freezeImmutable(current, desired, true)
		if *got.Ttl != 120 {
			t.Errorf("ttl = %d, want 120 (runtime-mutable)", *got.Ttl)
		}
		if len(got.Fs) != 1 {
			t.Errorf("fs len = %d, want 1 (runtime-mutable)", len(got.Fs))
		}
	})
}
