package isolation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGatewayForGuest(t *testing.T) {
	cases := map[string]string{
		"172.16.2.2": "172.16.2.1",
		"172.16.3.2": "172.16.3.1",
		"172.16.0.2": "172.16.0.1",
		"bogus":      "bogus", // malformed falls back to input
	}
	for in, want := range cases {
		if got := gatewayForGuest(in); got != want {
			t.Errorf("gatewayForGuest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMacForGuest(t *testing.T) {
	cases := map[string]string{
		"172.16.2.2":  "06:00:ac:10:02:02",
		"172.16.3.2":  "06:00:ac:10:03:02",
		"172.16.17.2": "06:00:ac:10:11:02",
		"bogus":       bootGuestMAC, // malformed falls back to the boot MAC
	}
	for in, want := range cases {
		if got := macForGuest(in); got != want {
			t.Errorf("macForGuest(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewMicroVMBootIdentity: a sandbox with no Key/GuestIP keeps the historical
// single-tenant identity (fixed boot network, hostname-keyed jail/tap/cgroup).
func TestNewMicroVMBootIdentity(t *testing.T) {
	t.Setenv("FIRECRACKER_RUN_DIR", "/run/fc")
	m := newMicroVM(Config{Hostname: "pod7"})

	if m.guestIP != bootGuestIP || m.gatewayIP != bootGatewayIP || m.guestMAC != bootGuestMAC {
		t.Errorf("boot net = %s/%s/%s, want %s/%s/%s", m.guestIP, m.gatewayIP, m.guestMAC, bootGuestIP, bootGatewayIP, bootGuestMAC)
	}
	if m.tapName != "fctap-pod7" {
		t.Errorf("tapName = %q, want fctap-pod7", m.tapName)
	}
	if m.jailDir != "/run/fc/pod7" {
		t.Errorf("jailDir = %q, want /run/fc/pod7", m.jailDir)
	}
	if m.cgroupPath != sandboxCgroupPath("pod7") {
		t.Errorf("cgroupPath = %q, want %q", m.cgroupPath, sandboxCgroupPath("pod7"))
	}
	if m.HasPrewarmSnapshot() {
		t.Error("boot sandbox should not claim a prewarm snapshot")
	}
}

// TestNewMicroVMPackedIdentity: a packed sandbox keeps the base snapshot's baked
// guest identity (it is never re-IP'd, design §7 "option 1") but derives a per-VM
// host identity from its allocated IP — its own netns, tap, jail, and SNAT
// sourceIP — so N coexist, and namespaces its cgroup by Key.
func TestNewMicroVMPackedIdentity(t *testing.T) {
	t.Setenv("FIRECRACKER_RUN_DIR", "/run/fc")
	m := newMicroVM(Config{Hostname: "pod7", Key: "alice", GuestIP: "172.16.3.2"})

	// The guest keeps the baked boot identity (no re-IP); 172.16.3.2 is only the
	// host-side SNAT source, exposed as sourceIP.
	if m.guestIP != bootGuestIP || m.gatewayIP != bootGatewayIP || m.guestMAC != bootGuestMAC {
		t.Errorf("packed guest net = %s/%s/%s, want baked %s/%s/%s",
			m.guestIP, m.gatewayIP, m.guestMAC, bootGuestIP, bootGatewayIP, bootGuestMAC)
	}
	if m.sourceIP != "172.16.3.2" {
		t.Errorf("sourceIP = %q, want 172.16.3.2", m.sourceIP)
	}
	if m.netnsName != "fcsbx3" {
		t.Errorf("netnsName = %q, want fcsbx3", m.netnsName)
	}
	if m.tapName != "fctap-3" {
		t.Errorf("tapName = %q, want fctap-3", m.tapName)
	}
	if m.jailDir != "/run/fc/3" {
		t.Errorf("jailDir = %q, want /run/fc/3", m.jailDir)
	}
	if want := sandboxCgroupPath("pod7-alice"); m.cgroupPath != want {
		t.Errorf("cgroupPath = %q, want %q", m.cgroupPath, want)
	}
	// No base files exist, so the packed VM doesn't take the resume fast path.
	if m.HasPrewarmSnapshot() {
		t.Error("packed sandbox without a ready base should not claim a prewarm snapshot")
	}
}

func TestBaseSnapshotReady(t *testing.T) {
	dir := t.TempDir()
	if baseSnapshotReady(dir) {
		t.Fatal("empty dir should not be ready")
	}
	for _, f := range []string{baseSnapshotName, baseMemName, baseOverlayName} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !baseSnapshotReady(dir) {
		t.Fatal("dir with all three non-empty artifacts should be ready")
	}
}
