package handlers

import (
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

func ptr[T any](v T) *T { return &v }

// entrypointArgv builds the generated union type from an argv slice.
func entrypointArgv(argv []string) *gen.SandboxConfig_Entrypoint {
	var e gen.SandboxConfig_Entrypoint
	if err := e.FromSandboxConfigEntrypoint1(argv); err != nil {
		panic(err)
	}
	return &e
}

func TestFreezeImmutable(t *testing.T) {
	// image is frozen whenever already set: an apply can't change it, only set
	// it when unset. (Isolation is no longer a config field — it's derived from
	// the image at boot.)
	t.Run("image frozen once set", func(t *testing.T) {
		current := gen.SandboxConfig{Image: ptr("base")}
		desired := gen.SandboxConfig{Image: ptr("other")}
		got := freezeImmutable(current, desired, false)
		if *got.Image != "base" {
			t.Errorf("image = %q, want base (frozen)", *got.Image)
		}
	})

	t.Run("image settable when unset", func(t *testing.T) {
		current := gen.SandboxConfig{}
		desired := gen.SandboxConfig{Image: ptr("base")}
		got := freezeImmutable(current, desired, false)
		if got.Image == nil || *got.Image != "base" {
			t.Errorf("image = %v, want base (settable)", got.Image)
		}
	})

	// cpu/memory/entrypoint/cwd/tty/env are settable while prewarm (not started)
	// and frozen afterward.
	t.Run("boot-time fields settable before start", func(t *testing.T) {
		current := gen.SandboxConfig{Cpu: ptr(1.0), Env: &map[string]string{"A": "1"}}
		desired := gen.SandboxConfig{Cpu: ptr(4.0), Env: &map[string]string{"A": "2"}}
		got := freezeImmutable(current, desired, false)
		if *got.Cpu != 4 {
			t.Errorf("cpu = %g, want 4 (settable before start)", *got.Cpu)
		}
		if (*got.Env)["A"] != "2" {
			t.Errorf("env A = %q, want 2 (settable before start)", (*got.Env)["A"])
		}
	})

	t.Run("boot-time fields frozen after start", func(t *testing.T) {
		current := gen.SandboxConfig{Cpu: ptr(1.0), Memory: ptr(512), Entrypoint: entrypointArgv([]string{"sh"}), Tty: ptr(true), Env: &map[string]string{"A": "1"}}
		desired := gen.SandboxConfig{Cpu: ptr(4.0), Memory: ptr(1024), Entrypoint: entrypointArgv([]string{"bash"}), Tty: ptr(false), Env: &map[string]string{"A": "2"}}
		got := freezeImmutable(current, desired, true)
		gotArgv, _ := got.Entrypoint.AsSandboxConfigEntrypoint1()
		if *got.Cpu != 1 || *got.Memory != 512 || len(gotArgv) != 1 || gotArgv[0] != "sh" || *got.Tty != true || (*got.Env)["A"] != "1" {
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

func TestValidateConfig(t *testing.T) {
	t.Run("accepts allow/deny egress access", func(t *testing.T) {
		cfg := gen.SandboxConfig{Egress: &[]gen.EgressRule{
			{Access: gen.EgressRuleAccessAllow, Host: "api.github.com"},
			{Access: gen.EgressRuleAccessDeny, Host: "*"},
		}}
		if err := validateConfig(cfg); err != nil {
			t.Errorf("validateConfig: unexpected error %v", err)
		}
	})

	// An empty (or otherwise unknown) access slips past Go's string typing but is
	// meaningless to the proxy and would make getConfig unreadable to typed
	// clients — reject it at apply time instead of persisting garbage.
	t.Run("rejects empty egress access", func(t *testing.T) {
		cfg := gen.SandboxConfig{Egress: &[]gen.EgressRule{
			{Access: "", Host: "news.ycombinator.com"},
		}}
		if err := validateConfig(cfg); err == nil {
			t.Error("validateConfig: want error for empty egress access, got nil")
		}
	})

	t.Run("rejects unknown egress access", func(t *testing.T) {
		cfg := gen.SandboxConfig{Egress: &[]gen.EgressRule{
			{Access: "allowed", Host: "example.com"},
		}}
		if err := validateConfig(cfg); err == nil {
			t.Error("validateConfig: want error for unknown egress access, got nil")
		}
	})

	t.Run("nil egress is valid", func(t *testing.T) {
		if err := validateConfig(gen.SandboxConfig{}); err != nil {
			t.Errorf("validateConfig: unexpected error %v", err)
		}
	})
}
