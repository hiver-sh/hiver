package firecracker

import (
	"encoding/json"
	"fmt"
	"os"
)

// Drive ids used in the boot config. The image rootfs is the read-only
// root device; the overlay drive carries the writable upper layer the
// guest stacks on top with overlayfs.
const (
	RootDriveID    = "rootfs"
	OverlayDriveID = "overlay"
)

// Config is the full pre-boot configuration written to Firecracker's
// --config-file. The JSON keys match Firecracker's config-file schema, so
// `firecracker --config-file <this>` boots the VM without any API calls;
// we still attach an API socket for post-boot control (state, snapshot).
type Config struct {
	BootSource        BootSource         `json:"boot-source"`
	Drives            []Drive            `json:"drives"`
	MachineConfig     MachineConfig      `json:"machine-config"`
	NetworkInterfaces []NetworkInterface `json:"network-interfaces,omitempty"`
	Vsock             *Vsock             `json:"vsock,omitempty"`
	Logger            *Logger            `json:"logger,omitempty"`
}

// WriteConfigFile marshals cfg to path for `firecracker --config-file`.
func WriteConfigFile(path string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Command returns the binary and args that boot the VM described by the
// config at configPath, exposing an API socket at apiSock for post-boot
// control. The caller runs it through its own process supervisor; when the
// process exits, the VM is down (mirroring `runc run`).
func Command(bin, apiSock, configPath string) (string, []string) {
	if bin == "" {
		bin = "firecracker"
	}
	return bin, []string{"--api-sock", apiSock, "--config-file", configPath}
}

// DefaultBootArgs is the kernel command line for a minimal serial-console
// guest whose init is the in-guest agent. ip=... hands the guest its
// static address on the tap link so it needs no in-guest DHCP. The agent
// receives its runtime parameters (overlay/FUSE/egress config) over vsock
// rather than the cmdline, so this stays fixed across sandboxes.
func DefaultBootArgs(guestIP, gatewayIP string) string {
	return fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd "+
			"ip=%s::%s:255.255.255.252::eth0:off "+
			"init=/usr/bin/sbxguest",
		guestIP, gatewayIP,
	)
}
