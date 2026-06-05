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
	"os/exec"

	"github.com/blasten/hive/internal/runc"
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

// Parse maps a config string (SandboxConfig.isolation) to a Kind. The
// empty string selects KindContainer so existing configs that predate the
// field keep their behaviour.
func Parse(s string) (Kind, error) {
	switch Kind(s) {
	case "", KindContainer:
		return KindContainer, nil
	case KindMicroVM:
		return KindMicroVM, nil
	default:
		return "", fmt.Errorf("unknown isolation %q (want %q or %q)", s, KindContainer, KindMicroVM)
	}
}

// Config carries the per-sandbox parameters a backend needs at
// construction time.
type Config struct {
	// Hostname is the sandbox-pod hostname (docker assigns one per
	// sandbox). Backends derive a unique cgroup path and container id
	// from it so multiple sandboxes on one host don't collide.
	Hostname string

	// LocalMounts lists each local-backend FUSE workspace as (agent path,
	// host backend dir). The container backend snapshots these dirs
	// directly; the microvm backend captures the guest's writable image
	// instead and ignores them.
	LocalMounts []SnapshotMount
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

// FileBridge exposes the workload's filesystem to the management API
// (/v1/file*). Both backends serve workspace paths from the host FUSE backend
// dirs (sbxfuse is host-side); the container backend also serves non-workspace
// paths from the overlay upper layer, while the microvm backend reports those
// as guest-only (they live in the guest's block device). Paths are
// agent-visible absolute paths; mounts is the configured FUSE mount list, used
// to route a path to its backing store.
type FileBridge interface {
	List(agentPath string, mounts []string) ([]FileEntry, error)
	Open(agentPath string, mounts []string) (rc io.ReadCloser, size int64, err error)
	Save(agentDir, name string, mounts []string, r io.Reader) (written int64, err error)
	// Stat reports a single entry; Name is the base name. Used by the MCP
	// file tools to distinguish files from directories.
	Stat(agentPath string, mounts []string) (FileEntry, error)
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
	// For the container backend the agent shares the host mount namespace, so
	// this is a no-op (runc bind-mounts it). For the microvm backend it
	// starts a 9p-over-vsock server rooted at the host mount and records the
	// vsock port so the guest can mount it. Call after the FUSE daemon is
	// mounted and ready.
	ExportWorkspace(ctx context.Context, mount string) error

	// InstallCA installs the sandbox CA (PEM) into the workload's trust
	// store so sbxproxy can terminate TLS. The container backend splices it
	// into the merged rootfs; the microvm backend hands it to the guest
	// agent, which installs it before the workload starts.
	InstallCA(certPEM []byte) error

	// RedirectEgress installs the firewall rules that funnel the
	// workload's outbound TCP to the in-pod proxy at proxyPort, exempting
	// sockets stamped with the given SO_MARK (the proxy's own upstream
	// traffic) so they aren't redirected back into the proxy.
	RedirectEgress(ctx context.Context, proxyPort, mark int) error


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
	switch kind {
	case KindContainer:
		return newContainer(cfg), nil
	case KindMicroVM:
		return newMicroVM(cfg), nil
	default:
		return nil, fmt.Errorf("unknown isolation kind %q", kind)
	}
}
