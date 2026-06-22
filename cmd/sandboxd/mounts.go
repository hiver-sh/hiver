package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/runc"
	"github.com/hiver-sh/hiver/internal/spec"
)

// fsSidecar identifies one live workspace mount in the pod-wide sbxfuse process:
// the host mount point it is keyed by in that process, and the ACL file we
// rewrite then ask the process to reload.
type fsSidecar struct {
	hostMount string
	aclPath   string
}

// mountManager owns the live set of sbxfuse workspace mounts and reconciles it
// toward a desired FS config: it starts a daemon for each newly-desired mount,
// stops the daemon for each mount no longer desired, and rewrites ACLs for the
// ones that stay. It is the single writer of the live set, so the same code
// path serves both the initial boot (every mount is "added") and a later
// PUT /v1/config (an arbitrary add/remove/keep delta).
//
// Surfacing an added/removed mount inside an already-running workload is
// backend-dependent: the container backend injects a newly-added mount into the
// live mount namespace (ExportWorkspace + ApplyResumeState) and detaches a removed
// one (UnexportWorkspace). The microvm guest needs live 9p hotplug/unmount
// cooperation that doesn't exist yet, so its updates reconcile the host side
// (sbxfuse + per-mount 9p server + ACLs + egress) but the guest only sees the
// mount set fixed at launch. Either way the cleanest case is reconciling before
// the agent launches (the warm-pod bring-up).
type mountManager struct {
	// Immutable deps captured at construction.
	ctx         context.Context
	children    *sync.WaitGroup
	broker      *events.Broker
	iso         isolation.Isolation
	isoMu       *sync.Mutex // serializes isolation-backend mutations (Export/Unexport)
	fuse        *fuseControl // pod-wide shared sbxfuse process (mount/unmount/reacl)
	workDir     string
	snapshotDir string
	soMark      int
	// keyPrefix namespaces the host-side workspace paths for a packed sandbox
	// (host mount + backend live under <keyPrefix>/...), so two sandboxes that
	// mount the same guest path (e.g. /workspace) don't collide. Empty for the
	// boot sandbox, which keeps the historical host==guest layout.
	keyPrefix string

	// reconcileMu serializes Reconcile so two callers firing on the same
	// config-apply event (the prewarm launch path and reconcileSidecars) can't
	// both decide a mount is missing and double-start its daemon.
	reconcileMu sync.Mutex
	// rootMounted is set once iso.MountRoot has assembled the overlay, after
	// which a snapshot can be restored into it. restored ensures the one
	// pre-launch restore happens at most once (CAS in maybeRestore).
	rootMounted atomic.Bool
	restored    atomic.Bool
	// workloadLive is set once the agent is running, after which a reconcile must
	// inject any newly-added workspace into the live workload (the launch-time
	// bundle/resume can't cover a mount added by a runtime config-apply).
	workloadLive atomic.Bool

	mu   sync.Mutex
	live map[string]fsSidecar // keyed by agent mount path
}

func newMountManager(ctx context.Context, children *sync.WaitGroup, broker *events.Broker, iso isolation.Isolation, isoMu *sync.Mutex, fuse *fuseControl, workDir, snapshotDir string, soMark int) *mountManager {
	return &mountManager{
		ctx:         ctx,
		children:    children,
		broker:      broker,
		iso:         iso,
		isoMu:       isoMu,
		fuse:        fuse,
		workDir:     workDir,
		snapshotDir: snapshotDir,
		soMark:      soMark,
		live:        map[string]fsSidecar{},
	}
}

// hostMount is the host path where sbxfuse mounts f, and the runc bind source.
// For the boot sandbox it is the guest path itself (host==guest); for a packed
// sandbox it is namespaced under keyPrefix so same-path mounts don't collide.
func (m *mountManager) hostMount(f spec.FS) string {
	if m.keyPrefix == "" {
		return f.Mount
	}
	return filepath.Join(m.keyPrefix, "mnt", f.Slug())
}

// hostBackend is the host directory sbxfuse writes through to for f.
func (m *mountManager) hostBackend(f spec.FS) string {
	if m.keyPrefix == "" {
		return f.BackendPath()
	}
	return filepath.Join(m.keyPrefix, "backend", f.Slug())
}

// aclFile is the per-mount ACL file path, namespaced by keyPrefix when packed.
func (m *mountManager) aclFile(f spec.FS) string {
	if m.keyPrefix == "" {
		return filepath.Join(m.workDir, "acls-"+f.Slug()+".json")
	}
	return filepath.Join(m.keyPrefix, "acls-"+f.Slug()+".json")
}

// defaultedACLs returns f.ACLs, falling back to a single read-write rule over
// the whole mount when none are set — matching spec.Validate, which the reconcile
// path (fed from on-disk config) doesn't run.
//
// Internal mounts are never exposed to the agent, so an ACL policy governing
// agent access is meaningless; the only accessor is sandboxd itself (e.g.
// snapshot capture/restore). They always get full read-write so that I/O is
// never blocked, regardless of any configured rules.
func defaultedACLs(f spec.FS) []fusefs.Rule {
	if f.Internal {
		return []fusefs.Rule{{Path: f.Mount + "/**", Access: fusefs.AccessRW}}
	}
	if len(f.ACLs) > 0 {
		return f.ACLs
	}
	return []fusefs.Rule{{Path: f.Mount + "/**", Access: fusefs.AccessRW}}
}

// aclsFor returns the ACL rules to write for f, rewriting each rule path's guest
// prefix (f.Mount) to the host mount point when packed. sbxfuse reconstructs the
// path it matches as <-mount> + relative, so a packed daemon mounted at a host
// path must carry host-prefixed ACL rules.
func (m *mountManager) aclsFor(f spec.FS) []fusefs.Rule {
	rules := defaultedACLs(f)
	host := m.hostMount(f)
	if m.keyPrefix == "" || host == f.Mount {
		return rules
	}
	out := make([]fusefs.Rule, len(rules))
	for i, r := range rules {
		r.Path = host + strings.TrimPrefix(r.Path, f.Mount)
		out[i] = r
	}
	return out
}

// start brings up one sbxfuse daemon for f and exposes it to the workload. It
// mirrors the boot-time fuse setup so boot and reconcile share one path.
func (m *mountManager) start(f spec.FS) error {
	mountPoint := m.hostMount(f)
	backend := m.hostBackend(f)
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		// A re-created key can find a stale FUSE mountpoint here: a prior sandbox
		// with the same key whose async teardown didn't fully clear it leaves the
		// path present but un-stat-able (dead transport endpoint), so MkdirAll
		// fails with EEXIST. Force-detach and clear it, then retry.
		if cerr := clearStaleMount(mountPoint); cerr != nil {
			return fmt.Errorf("create mount point %s: %w (clear stale: %v)", mountPoint, err, cerr)
		}
		if err := os.MkdirAll(mountPoint, 0o755); err != nil {
			return fmt.Errorf("create mount point %s: %w", mountPoint, err)
		}
	}
	if err := os.MkdirAll(backend, 0o755); err != nil {
		return fmt.Errorf("create backend %s: %w", backend, err)
	}

	// Seed the workspace from the image: move any files the image ships at the
	// mount path into the FUSE backend so the agent sees them. Skipped for a
	// packed sandbox — the image rootfs (overlay lower) is shared across all
	// packed sandboxes, so a destructive move would steal the content from peers;
	// a packed workspace starts empty.
	if !f.Internal && m.keyPrefix == "" {
		if err := isolation.SeedWorkspace(backend, filepath.Join(runc.RootfsDir, f.Mount)); err != nil {
			return fmt.Errorf("seed workspace %s: %w", f.Mount, err)
		}
	}

	aclPath := m.aclFile(f)
	if err := writeACLs(aclPath, m.aclsFor(f)); err != nil {
		return fmt.Errorf("write ACLs (%s): %w", f.Mount, err)
	}

	spec := fuseMountSpec{Mount: mountPoint, Backend: backend, ACLs: aclPath}
	if f.Backend.IsRemote() {
		blob, err := f.BackendConfigJSON()
		if err != nil {
			return fmt.Errorf("backend %q config (%s): %w", f.Backend, f.Mount, err)
		}
		spec.Remote = string(f.Backend)
		spec.RemoteConfig = string(blob)
		spec.Mark = m.soMark
	}

	// Register before issuing the mount so this mount's audit events resolve to
	// this sandbox's broker the instant sbxfuse starts emitting them.
	m.fuse.trans.register(mountPoint, f.Mount, gen.Backend(f.Backend), m.broker)
	if err := m.fuse.Mount(spec); err != nil {
		m.fuse.trans.unregister(mountPoint)
		return fmt.Errorf("start fuse (%s): %w", f.Mount, err)
	}
	if err := waitForMountReady(m.ctx, mountPoint, mountReadTimout); err != nil {
		_ = m.fuse.Unmount(mountPoint)
		m.fuse.trans.unregister(mountPoint)
		return fmt.Errorf("fuse did not mount %s: %w", mountPoint, err)
	}

	// Internal mounts stay host-side only: the sbxfuse daemon is mounted on the
	// sandbox host (so sandboxd can read/write it — e.g. as a snapshot target)
	// but it is never exported into the agent workload, so the agent never sees
	// it. Everything else is exported into the workload's mount namespace.
	if !f.Internal {
		m.isoMu.Lock()
		err := m.iso.ExportWorkspace(m.ctx, mountPoint, f.Mount)
		m.isoMu.Unlock()
		if err != nil {
			_ = m.fuse.Unmount(mountPoint)
			m.fuse.trans.unregister(mountPoint)
			return fmt.Errorf("export workspace %s: %w", mountPoint, err)
		}
	}

	m.mu.Lock()
	m.live[f.Mount] = fsSidecar{hostMount: mountPoint, aclPath: aclPath}
	m.mu.Unlock()
	return nil
}

// stop tears down the workspace mount in the shared sbxfuse process and reverses
// its export. The control-channel unmount lets sbxfuse fusermount -u (and drain a
// remote oplog) for just this mount, leaving the pod's other mounts running. A
// no-op for an unknown mount.
func (m *mountManager) stop(mount string) {
	m.mu.Lock()
	sc, ok := m.live[mount]
	if ok {
		delete(m.live, mount)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	// Detach the mount from the workload first (for a live container this unmounts
	// it from the agent's mount namespace), then tear down the host sbxfuse daemon
	// that was serving it. Both are best-effort: a failure to detach the consumer
	// must not strand the host-side daemon.
	m.isoMu.Lock()
	err := m.iso.UnexportWorkspace(m.ctx, mount)
	m.isoMu.Unlock()
	if err != nil {
		log.Printf("sandboxd: reconcile: unexport %s: %v", mount, err)
	}
	if err := m.fuse.Unmount(sc.hostMount); err != nil {
		log.Printf("sandboxd: reconcile: unmount sbxfuse (mount=%s host=%s): %v", mount, sc.hostMount, err)
	}
	m.fuse.trans.unregister(sc.hostMount)
	_ = os.Remove(sc.aclPath)
}

// reACL rewrites a kept mount's ACL file and tells the shared sbxfuse to reload
// it for that mount.
func (m *mountManager) reACL(f spec.FS) error {
	m.mu.Lock()
	sc, ok := m.live[f.Mount]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if err := writeACLs(sc.aclPath, m.aclsFor(f)); err != nil {
		return fmt.Errorf("reconcile acls (%s): %w", f.Mount, err)
	}
	if err := m.fuse.ReACL(sc.hostMount); err != nil {
		return fmt.Errorf("reload acls sbxfuse (mount=%s host=%s): %w", f.Mount, sc.hostMount, err)
	}
	return nil
}

// stopAll tears down every live workspace mount. Used on packed-sandbox teardown,
// where the shared sbxfuse process outlives the sandbox, so its mounts must be
// removed explicitly (cancelling the sandbox ctx doesn't reach them).
func (m *mountManager) stopAll() {
	m.mu.Lock()
	mounts := make([]string, 0, len(m.live))
	for mt := range m.live {
		mounts = append(mounts, mt)
	}
	m.mu.Unlock()
	for _, mt := range mounts {
		m.stop(mt)
	}
}
