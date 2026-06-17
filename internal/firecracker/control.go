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
}

// ControlResponse is the guest→host reply to a ControlRequest.
type ControlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
