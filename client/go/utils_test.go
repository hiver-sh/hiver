package client

import "testing"

// The pinned body replaces the nested create's request body verbatim, skipping
// GetOrCreateSandbox's client-side defaulting. Without the same defaults a
// config with no FS creates a sandbox with NO workspace — while a resumed VM
// snapshot still holds its 9p /workspace mount, which is then orphaned and
// wedges the first process to touch it in p9_client_rpc.
func TestAllowSandbox_PinsDefaultWorkspaceFS(t *testing.T) {
	rules := AllowSandbox("worker-1", SandboxConfig{Image: "browser"})

	var bodies []SandboxConfig
	for _, r := range rules {
		if r.Override != nil && r.Override.BodyStrategy == BodyStrategyReplace {
			cfg, ok := r.Override.Body.(SandboxConfig)
			if !ok {
				t.Fatalf("pinned body is %T, want SandboxConfig", r.Override.Body)
			}
			bodies = append(bodies, cfg)
		}
	}
	if len(bodies) != 2 { // docker + k8s gateway hosts
		t.Fatalf("got %d create rules with a pinned body, want 2", len(bodies))
	}
	for _, cfg := range bodies {
		if len(cfg.FS) != 1 || cfg.FS[0].Mount != "/workspace" || cfg.FS[0].Backend != "local" {
			t.Errorf("pinned FS = %+v, want the default /workspace local mount", cfg.FS)
		}
		if len(cfg.Egress) != 1 || cfg.Egress[0].Host != "*" || cfg.Egress[0].Access != "allow" {
			t.Errorf("pinned Egress = %+v, want the default allow-all rule", cfg.Egress)
		}
		if cfg.Image != "browser" {
			t.Errorf("pinned Image = %q, want %q", cfg.Image, "browser")
		}
	}
}

func TestAllowSandbox_KeepsExplicitFS(t *testing.T) {
	explicit := []FileSystem{{Mount: "/data", Backend: "local"}}
	rules := AllowSandbox("worker-1", SandboxConfig{FS: explicit})
	for _, r := range rules {
		if r.Override == nil {
			continue
		}
		cfg := r.Override.Body.(SandboxConfig)
		if len(cfg.FS) != 1 || cfg.FS[0].Mount != "/data" {
			t.Errorf("pinned FS = %+v, want the explicit /data mount", cfg.FS)
		}
	}
}
