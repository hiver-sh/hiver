// Package isolation abstracts the mechanism that confines and runs the
// agent workload, keeping sandboxd and the API handlers independent of
// the underlying runtime.
//
// Two backends are provided:
//
//   - "container" (the default) runs the agent with runc. Every isolation
//     primitive — overlayfs, FUSE mounts, iptables, the cgroup, and exec —
//     is a host-level operation that the container shares via namespaces.
//   - "microvm" runs the agent inside a firecracker guest with its own
//     kernel. sbxfuse and sbxproxy still run on the host; the guest reaches
//     them over virtio: workspaces via 9p-over-vsock (rooted at the host
//     sbxfuse mount), egress via a tap device the host REDIRECTs, and exec
//     over a vsock bridge. The root filesystem is a block device.
//
// Keeping sbxfuse/sbxproxy host-side means both backends reuse the same FUSE
// daemons, ACLs, audit events, and reconcile path — only the transport into
// the workload differs, which is what this interface abstracts.
package isolation

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/hiver-sh/hiver/internal/runc"
)

// NodeCACertPath is where each backend installs a standalone copy of the
// sandbox CA inside the workload rootfs. Node.js uses a bundled Mozilla root
// store and ignores the system bundle, so sandboxd points
// NODE_EXTRA_CA_CERTS here for TLS interception to succeed.
const NodeCACertPath = "/etc/ssl/certs/sandbox-ca.crt"

// Kind selects an isolation backend. The zero value is invalid; callers
// should run a config string through [Parse], which defaults to
// [KindContainer].
type Kind string

const (
	KindContainer Kind = "container"
	KindMicroVM   Kind = "microvm"
)

// Default compute allocation applied when a SandboxConfig omits cpu/memory.
// Both backends use these: the microvm as the guest vCPU count / RAM size, the
// container as a CPU quota / cgroup memory limit.
const (
	DefaultVcpuCount = 1   // virtual CPUs
	DefaultMemoryMiB = 512 // MiB
)

// kvmDevice is the host device a microVM needs: the KVM hypervisor interface.
// Its presence (and openability) is what makes microvm isolation possible.
const kvmDevice = "/dev/kvm"

// Detect selects the isolation backend automatically from the image contents,
// rather than from any config field. A microvm image ships a guest root
// filesystem image at [bundledRootfsImg]; its presence selects KindMicroVM,
// which additionally requires KVM — so when the rootfs image is present but KVM
// is unavailable, Detect returns a user-facing error rather than silently
// downgrading. Any image without that rootfs runs as a plain KindContainer.
func Detect() (Kind, error) {
	info, err := os.Stat(bundledRootfsImg)
	switch {
	case err == nil && info.Size() > 0:
		// microvm image — confirm the host can actually run a guest.
		if err := checkKVM(); err != nil {
			return "", err
		}
		return KindMicroVM, nil
	case err == nil:
		// A present-but-empty rootfs image is not a usable microvm image; treat
		// it as a container so a malformed bundle fails later with a clearer error.
		return KindContainer, nil
	case os.IsNotExist(err):
		return KindContainer, nil
	default:
		return "", fmt.Errorf("detect isolation: stat %s: %w", bundledRootfsImg, err)
	}
}

// checkKVM reports whether the host exposes a usable KVM device, returning a
// user-friendly error when it doesn't. A microvm image cannot boot without it.
func checkKVM() error {
	f, err := os.OpenFile(kvmDevice, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("this sandbox image requires microVM isolation, which needs hardware virtualization via %s, but that device is not present. Run the sandbox on a host with KVM enabled (bare metal or a VM with nested virtualization), or use a container-isolation image", kvmDevice)
		}
		return fmt.Errorf("this sandbox image requires microVM isolation via %s, but it could not be opened (%v). Ensure the sandbox has access to %s", kvmDevice, err, kvmDevice)
	}
	_ = f.Close()
	return nil
}

// Config carries the per-sandbox parameters a backend needs at
// construction time.
type Config struct {
	// Hostname is the sandbox-pod hostname (docker assigns one per
	// sandbox). Backends derive a unique cgroup path and container id
	// from it so multiple sandboxes on one host don't collide.
	Hostname string

	// Key is the sandbox key when more than one sandbox is packed into a single
	// pod (design §6). Empty for the boot sandbox, which keeps the historical
	// single-sandbox layout. When set, backends namespace their per-sandbox
	// runtime state (runc root/cgroup/container id; microvm jail) by it.
	Key string

	// GuestIP is the pod-local IP assigned to a packed sandbox (e.g.
	// "172.16.0.2"). When set, the container backend runs the workload in its
	// own network namespace with this IP so its egress carries a distinct source
	// address (design §6/§8); the shared sbxproxy then applies per-source rules.
	// Empty keeps the workload in the shared pod netns (the boot sandbox).
	GuestIP string

	// BaseSnapshotDir, when set, is the shared per-image base snapshot a packed
	// microvm resumes from instead of cold-booting (design §7): all VMs in the pod
	// LoadSnapshot the same base/{snapshot.bin, mem.bin} (guest memory shared COW
	// via the File backend) with a per-VM CoW overlay, tap, and vsock. Only the
	// microvm backend uses it; the container backend ignores it. Empty cold-boots.
	BaseSnapshotDir string

	// LocalMounts lists each local-backend FUSE workspace as (agent path,
	// host backend dir). The container backend snapshots these dirs
	// directly; the microvm backend captures the guest's writable image
	// instead and ignores them.
	LocalMounts []SnapshotMount

	// VcpuCount and MemoryMiB are the compute allocation for the sandbox,
	// fixed at boot (SandboxConfig.cpu / .memory). The microvm backend boots
	// the guest with exactly these; the container backend enforces them as a
	// CPU quota and cgroup memory limit. New fills zero values with
	// DefaultVcpuCount / DefaultMemoryMiB.
	VcpuCount int
	MemoryMiB int
}

// SnapshotMount pairs an agent-visible mount path with the host directory
// that backs it, so snapshot capture/restore can route local-backend FUSE
// data to the right place.
type SnapshotMount struct {
	ContainerPath string
	HostDir       string
}

// AgentConfig carries everything a backend needs to launch the agent
// workload. The image config and bind mounts reuse the runc helper types
// since image delivery (a docker archive under /mnt) is shared across
// backends.
type AgentConfig struct {
	ImageConfig *runc.ImageConfig
	Env         map[string]string
	Mounts      []runc.BindMount
	Hostname    string
	// TTY, when set, launches the entrypoint attached to a pseudo-terminal so
	// a client can attach to it via /v1/exec-stream. The caller wires the
	// returned command's stdio to a pty (see internal/pty). Only the container
	// backend honours this; the microvm backend ignores it.
	TTY bool

	// Prewarm marks a prewarm launch: the workload runs as usual but readiness
	// is delayed until it writes /run/hiver/prewarm-ready, so the host snapshots
	// (microvm) or adopts (container) a warm workload. Set only by PrewarmSnapshot.
	Prewarm bool
}

// ExecConfig describes a command to run inside the running workload.
type ExecConfig struct {
	Command string
	Cwd     *string
	Env     *map[string]string
	TTY     bool
}

// FileEntry is one directory child returned by FileBridge.List.
type FileEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// MountRoute describes one configured FUSE mount for path routing in the
// FileBridge. Remote reports whether the backend writes through to a remote
// store (gdrive, gcs); for those the mount's "-backend" dir is only a write
// buffer the oplog evicts after flushing, so the file API must read the FUSE
// mount point (the merged remote+local view) instead.
type MountRoute struct {
	Mount  string
	Remote bool
}

// FileBridge exposes the workload's filesystem to the management API
// (/v1/file*). For local-backend mounts both isolation backends serve workspace
// paths from the host FUSE backend dir directly (sbxfuse is host-side),
// bypassing ACLs — the file API is a higher-privilege control surface. For
// remote-backed mounts the backend dir is only a write buffer, so reads/writes
// route through the FUSE mount point instead. The container backend also serves
// non-workspace paths from the overlay upper layer, while the microvm backend
// reports those as guest-only (they live in the guest's block device). Paths are
// agent-visible absolute paths; mounts is the configured mount list, used to
// route a path to its backing store.
type FileBridge interface {
	List(agentPath string, mounts []MountRoute) ([]FileEntry, error)
	Open(agentPath string, mounts []MountRoute) (rc io.ReadCloser, size int64, err error)
	Save(agentDir, name string, mounts []MountRoute, r io.Reader) (written int64, err error)
	// Stat reports a single entry; Name is the base name. Used by the MCP
	// file tools to distinguish files from directories.
	Stat(agentPath string, mounts []MountRoute) (FileEntry, error)
	// Delete removes a single file or empty directory at agentPath.
	Delete(agentPath string, mounts []MountRoute) error
}

// Isolation is the polymorphic runtime boundary. A single instance is
// constructed per sandbox by [New] and shared between sandboxd (which
// assembles the filesystem, network, and cgroup, then launches the agent)
// and the API handlers (which exec into the running workload).
type Isolation interface {
	// Kind reports which backend this is.
	Kind() Kind

	// MountRoot assembles the agent root filesystem: an overlay with
	// lower=image (read-only), upper=sandbox writes, merged=workload root.
	// Must be called after any FUSE backends are seeded so the overlay
	// lower layer reflects clean image content.
	MountRoot() error

	// UnmountRoot tears the overlay stack down in reverse order. Safe to
	// call after the workload exits.
	UnmountRoot() error

	// ExportWorkspace makes the host sbxfuse mount visible to the workload.
	// hostMount is the host-side FUSE path; guestMount is the path the agent
	// sees inside the workload (they differ for packed sandboxes). For the
	// container backend at launch the agent shares the host mount namespace
	// (runc bind-mounts it using the two paths); one added after launch is
	// injected via bindWorkspaceIntoContainer. For the microvm backend it
	// starts a 9p-over-vsock server rooted at the host mount. Call after the
	// FUSE daemon is mounted and ready.
	ExportWorkspace(ctx context.Context, hostMount, guestMount string) error

	// UnexportWorkspace reverses ExportWorkspace for a mount removed from the
	// config at runtime (config-apply reconcile). The container backend forgets
	// the workspace and, if it had already been injected into the running agent,
	// detaches it from the container's mount namespace so the agent stops seeing
	// it (unmountWorkspaceFromContainer). The microvm backend stops the mount's
	// 9p-over-vsock server; the guest keeps the mount it saw at launch until a
	// live guest-side unmount exists. Call before tearing down the host FUSE
	// daemon that served the mount.
	UnexportWorkspace(ctx context.Context, mount string) error

	// InstallCA installs the sandbox CA (PEM) into the workload's trust
	// store so sbxproxy can terminate TLS. The container backend splices it
	// into the merged rootfs; the microvm backend hands it to the guest
	// agent, which installs it before the workload starts.
	InstallCA(certPEM []byte) error

	// RedirectEgress installs the firewall rules that funnel the workload's
	// outbound traffic to the in-pod sidecars: TCP to the proxy at proxyPort,
	// and all DNS (UDP/53 and TCP/53, to any resolver) to the DNS sinkhole at
	// dnsPort. Sockets stamped with the given SO_MARK (the proxy's own upstream
	// and resolver traffic) are exempted so they aren't redirected back.
	RedirectEgress(ctx context.Context, proxyPort, dnsPort, mark int) error

	// CgroupPath is the absolute cgroup the workload runs under, used both
	// to confine it (written into the runtime config) and to read resource
	// usage from /sys/fs/cgroup<CgroupPath>.
	CgroupPath() string

	// RestoreSnapshot restores a previously captured writable state before
	// the workload starts; CaptureSnapshot persists it on shutdown. include
	// is the set of agent paths to capture. Both backends produce the same
	// gzip-tar format (the microvm backend loop-mounts its overlay image and
	// runs the same snapshot package).
	RestoreSnapshot(src string) error
	CaptureSnapshot(dst string, include []string) error

	// LaunchAgent prepares any runtime config the backend needs and
	// returns the command (bin + args) that starts the agent workload.
	// sandboxd runs it through its own child supervisor, so the command is
	// returned rather than started here.
	LaunchAgent(cfg AgentConfig) (bin string, args []string, err error)

	// WaitReady blocks until the workload is running or ctx is cancelled.
	WaitReady(ctx context.Context) error

	// PrewarmSnapshot brings up the image entrypoint off the claim path so a
	// claimed sandbox inherits a warm, already-running workload. The microvm
	// backend boots a guest running the entrypoint, waits for it to write
	// /run/hiver/prewarm-ready, pauses it, writes a full VM snapshot, and stops
	// the transient guest. The container backend starts the runc container
	// running the entrypoint and keeps it alive (gated on its poststart fifo).
	// Either way the entrypoint runs before the config is known, so it must be
	// config-independent (a resident service like the browser host). On failure a
	// backend degrades to cold launch (HasPrewarmSnapshot stays false). Call after
	// MountRoot/InstallCA and before awaiting the first config.
	PrewarmSnapshot(ctx context.Context, imgCfg *runc.ImageConfig) error

	// HasPrewarmSnapshot reports whether PrewarmSnapshot succeeded and the resume
	// path (below) should be taken on claim instead of a cold launch.
	HasPrewarmSnapshot() bool

	// ResumeAgent returns the command that starts the resume process. For the
	// microvm backend that is a fresh VMM (no boot config — the machine state
	// comes from the snapshot, loaded by ResumeReady once its API socket is up),
	// supervised like LaunchAgent's command. The container backend's workload is
	// already running from PrewarmSnapshot, so it returns an empty command and the
	// caller starts no child. Only valid after a successful PrewarmSnapshot.
	ResumeAgent() (bin string, args []string, err error)

	// ResumeReady makes the resumed workload serving: the microvm backend loads
	// the snapshot into the VMM started by ResumeAgent and resumes the guest; the
	// container backend is already serving and returns nil. Resume-path analogue
	// of WaitReady.
	ResumeReady(ctx context.Context) error

	// ApplyResumeState delivers post-resume setup the prewarm step could not
	// carry: the config's workspaces (microvm: their 9p-over-vsock mounts don't
	// survive a snapshot/restore; container: bind them into the running mount ns)
	// and, for the microvm, the workload environment + clock fix. The already-
	// running entrypoint doesn't pick up late env, so env matters only for exec
	// sessions; unknown when the workload was prewarmed.
	ApplyResumeState(ctx context.Context, env []string) error

	// StopAgent stops a workload started by PrewarmSnapshot on the resume teardown
	// path. The container backend kills + deletes the running runc container; the
	// microvm backend is a no-op (its VMM child is stopped by cancelling the
	// supervising context instead).
	StopAgent(ctx context.Context) error

	// FlushAgent flushes the running workload's filesystem so its recent
	// writes are durable before the workload is stopped and a snapshot
	// captured. The microvm backend syncs the guest, whose writes are
	// otherwise trapped in the guest page cache and lost when the VM is killed;
	// the container backend is a no-op (its overlay upper is a host directory).
	FlushAgent(ctx context.Context) error

	// ExecCmd builds (but does not start) an *exec.Cmd that runs cfg
	// inside the running workload. The caller wires stdin/stdout/stderr
	// (attaching a pty or pipes) and calls Start. The returned cleanup
	// func reaps the in-workload process tree if ctx was cancelled (client
	// abort) and removes scratch state; the caller must defer it.
	ExecCmd(ctx context.Context, cfg ExecConfig) (cmd *exec.Cmd, cleanup func(), err error)

	// Files exposes the workload filesystem to the /v1/file* handlers.
	Files() FileBridge
}

// sandboxCgroupPath is the absolute cgroup the agent's resource usage is
// accounted under, derived from the pod hostname so sandboxes sharing a host
// don't collide. Both backends place the workload here — runc via the bundle
// config, the microvm by moving the firecracker VMM into it — and
// PollResourceUsage reads /sys/fs/cgroup<path>. The pod runs with --cgroupns
// host and a writable /sys/fs/cgroup, so this is a real host cgroup path.
func sandboxCgroupPath(hostname string) string { return "/sandbox-" + hostname }

// New constructs the isolation backend selected by kind.
func New(kind Kind, cfg Config) (Isolation, error) {
	if cfg.VcpuCount <= 0 {
		cfg.VcpuCount = DefaultVcpuCount
	}
	if cfg.MemoryMiB <= 0 {
		cfg.MemoryMiB = DefaultMemoryMiB
	}
	switch kind {
	case KindContainer:
		return newContainer(cfg), nil
	case KindMicroVM:
		return newMicroVM(cfg), nil
	default:
		return nil, fmt.Errorf("unknown isolation kind %q", kind)
	}
}
