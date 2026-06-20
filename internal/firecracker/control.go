package firecracker

// GuestControlPort is the guest vsock port the in-guest agent (cmd/sbxguest)
// listens on for host-issued control RPCs. It exists for the snapshot-resume
// flow: a prewarm VM is snapshotted before its first config is known, so the
// workspaces that config names cannot be mounted at boot. After the host loads
// and resumes the snapshot it dials this port (host→guest, via DialGuest) to
// drive post-resume setup that could not be baked into the params drive.
//
// One request/response per connection, each a single JSON object terminated by
// a newline (see ControlRequest/ControlResponse).
const GuestControlPort uint32 = 1028

// ControlRequest is the host→guest control message carrying the post-resume
// setup the params drive could not: a prewarm VM is snapshotted before its first
// config is known, so neither the workload environment nor the config's
// workspaces exist at boot.
type ControlRequest struct {
	// Env is the workload environment ("KEY=VALUE" entries) the guest applies to
	// its own process so binary resolution (exec.LookPath) and os.Environ() in
	// exec sessions work — a prewarm guest boots with an empty environment (no
	// PATH), so without this the entrypoint and every exec fail to resolve their
	// command. Mirrors the params.Env application a cold boot does at startup.
	Env []string `json:"env,omitempty"`

	// MountWorkspaces, when non-empty, asks the guest to mount these
	// 9p-over-vsock workspaces into the already-running guest. Used after a
	// snapshot resume, since the active vsock connections backing 9p mounts do
	// not survive a snapshot/restore and the workspaces are not known until the
	// first config arrives.
	MountWorkspaces []GuestFuse `json:"mount_workspaces,omitempty"`

	// UnmountWorkspaces, when non-empty, asks the guest to unmount these workspace
	// paths — used when a config-apply removes a mount from a running sandbox. The
	// host stops serving the 9p export, but the guest must also umount the (now
	// dead) mountpoint and remove its directory, or the path lingers in the guest
	// (the runc backend does the equivalent live umount). Guest paths, matching the
	// MountWorkspaces[].Mount the guest mounted at.
	UnmountWorkspaces []string `json:"unmount_workspaces,omitempty"`

	// UnixNano, when non-zero, is the host wall-clock time (nanoseconds since the
	// Unix epoch) the guest should set its clock to. A snapshot resume restores
	// the guest clock to capture time and never resyncs, so a warm pod's clock
	// lags real time by however long it sat before being claimed — which makes
	// freshly-minted TLS leaf certs look "not yet valid" (ERR_CERT_DATE_INVALID)
	// to in-guest clients. Setting it here, before the workload runs, corrects it.
	UnixNano int64 `json:"unix_nano,omitempty"`

	// GuestIP/GatewayIP, when set, re-address the guest's eth0 after a pack-mode
	// resume. Every VM resumes from the same per-image base snapshot, whose kernel
	// cmdline baked in the base builder's address — so each resumed guest comes up
	// on that stale IP and must be re-IP'd to its own pod-local /30 (172.16.<n>.2,
	// gateway 172.16.<n>.1) before any egress, so sbxproxy's per-source ACL sees a
	// distinct source. GuestIP is the bare address (the /30 prefix is implied).
	// Empty on the in-place (non-pack) resume path, which keeps its booted address.
	GuestIP   string `json:"guest_ip,omitempty"`
	GatewayIP string `json:"gateway_ip,omitempty"`
}

// ControlResponse is the guest→host reply to a ControlRequest.
type ControlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
