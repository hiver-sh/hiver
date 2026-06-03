package firecracker

// GuestParams is the runtime configuration the host hands the in-guest
// agent (cmd/sbxguest) via the read-only metadata drive. The agent reads it
// from ParamsPath at boot to assemble the overlay + FUSE mounts, install the
// in-guest egress firewall, and launch the workload.
//
// It is the host↔guest contract: the host (microvm isolation backend)
// writes it, the guest agent consumes it. Keep the two in lockstep.
type GuestParams struct {
	// Entrypoint+Cmd+Env+WorkingDir come from the agent image config and
	// describe the workload process the guest agent ultimately execs as
	// PID-equivalent inside the assembled root.
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
	Env        []string `json:"env"`
	WorkingDir string   `json:"working_dir"`

	// Fuse lists the workspaces the guest mounts over 9p-over-vsock before
	// starting the workload. The sbxfuse daemons run on the host; each mount
	// is exported on its own vsock port (GuestFuse.Port) which the guest
	// dials and mounts with trans=fd 9p, so every workspace op lands on the
	// host FUSE daemon (preserving its ACLs, audit, and remote backends).
	Fuse []GuestFuse `json:"fuse,omitempty"`

	// ProxyPort/Mark configure the in-guest egress firewall: outbound TCP
	// is REDIRECTed to ProxyPort (the host sbxproxy, reached over the tap
	// link), exempting sockets stamped with Mark.
	ProxyPort int `json:"proxy_port"`
	Mark      int `json:"mark"`

	// ProxyAddr is the host:port the guest routes egress to (the host end
	// of the tap link plus the proxy port).
	ProxyAddr string `json:"proxy_addr"`

	// CACertPEM is the sandbox CA the guest agent splices into the
	// workload's trust store so sbxproxy can terminate TLS.
	CACertPEM []byte `json:"ca_cert_pem,omitempty"`

	// EtcHosts/EtcResolvConf carry the host's /etc/hosts and
	// /etc/resolv.conf so the guest workload resolves names the same way a
	// shared-netns container would (the container backend bind-mounts these).
	EtcHosts       []byte `json:"etc_hosts,omitempty"`
	EtcResolvConf  []byte `json:"etc_resolv_conf,omitempty"`
	NodeCACertPath string `json:"node_ca_cert_path,omitempty"`
}

// GuestFuse describes one workspace the guest mounts over 9p. Port is the
// guest→host vsock port the host 9p server (rooted at the host sbxfuse mount)
// listens on for this workspace.
type GuestFuse struct {
	Mount   string `json:"mount"`
	Backend string `json:"backend"`
	Port    uint32 `json:"port"`
}

// ParamsPath is where the guest agent expects the params file once it has
// mounted the metadata drive (PUT as the last drive, RootDriveID excluded).
const ParamsPath = "/run/sbxguest/params.json"

// MetadataDriveID is the drive id of the read-only params/metadata drive.
const MetadataDriveID = "metadata"
