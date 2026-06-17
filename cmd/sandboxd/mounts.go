package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/runc"
	"github.com/hiver-sh/hiver/internal/spec"
)

// fsSidecar identifies one running sbxfuse process: the pid we signal and the
// ACL file we rewrite for it.
type fsSidecar struct {
	pid     int
	aclPath string
}

// mountManager owns the live set of sbxfuse workspace mounts and reconciles it
// toward a desired FS config: it starts a daemon for each newly-desired mount,
// stops the daemon for each mount no longer desired, and rewrites ACLs for the
// ones that stay. It is the single writer of the live set, so the same code
// path serves both the initial boot (every mount is "added") and a later
// PUT /v1/config (an arbitrary add/remove/keep delta).
//
// Surfacing an added/removed mount inside an already-running workload is
// backend-limited (see isolation.UnexportWorkspace): for the microvm guest a
// live hotplug/unmount needs guest cooperation that doesn't exist yet, and for
// the container the bind is fixed at runc launch. So the fully-correct case is
// reconciling before the agent launches (the warm-pod bring-up); a live update
// reconciles the host side (sbxfuse + ACLs + egress) regardless.
type mountManager struct {
	// Immutable deps captured at construction.
	ctx         context.Context
	children    *sync.WaitGroup
	broker      *events.Broker
	iso         isolation.Isolation
	isoMu       *sync.Mutex // serializes isolation-backend mutations (Export/Unexport)
	fuseBin     string
	workDir     string
	snapshotDir string
	soMark      int

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

func newMountManager(ctx context.Context, children *sync.WaitGroup, broker *events.Broker, iso isolation.Isolation, isoMu *sync.Mutex, fuseBin, workDir, snapshotDir string, soMark int) *mountManager {
	return &mountManager{
		ctx:         ctx,
		children:    children,
		broker:      broker,
		iso:         iso,
		isoMu:       isoMu,
		fuseBin:     fuseBin,
		workDir:     workDir,
		snapshotDir: snapshotDir,
		soMark:      soMark,
		live:        map[string]fsSidecar{},
	}
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

// start brings up one sbxfuse daemon for f and exposes it to the workload. It
// mirrors the boot-time fuse setup so boot and reconcile share one path.
func (m *mountManager) start(f spec.FS) error {
	if err := os.MkdirAll(f.Mount, 0o755); err != nil {
		return fmt.Errorf("create mount point %s: %w", f.Mount, err)
	}
	if err := os.MkdirAll(f.BackendPath(), 0o755); err != nil {
		return fmt.Errorf("create backend %s: %w", f.BackendPath(), err)
	}

	// Seed the workspace from the image: move any files the image ships at the
	// mount path into the FUSE backend so the agent sees them. Done here, in the
	// reconciler, so a mount added by a later config change is seeded too. It must
	// precede iso.MountRoot (which the caller runs after the reconcile) — the seed
	// empties these paths out of the overlay lower. Internal mounts are never
	// exposed to the agent and the image carries no content for them, so skip it.
	if !f.Internal {
		if err := isolation.SeedWorkspace(f.BackendPath(), filepath.Join(runc.RootfsDir, f.Mount)); err != nil {
			return fmt.Errorf("seed workspace %s: %w", f.Mount, err)
		}
	}

	aclPath := filepath.Join(m.workDir, "acls-"+f.Slug()+".json")
	if err := writeACLs(aclPath, defaultedACLs(f)); err != nil {
		return fmt.Errorf("write ACLs (%s): %w", f.Mount, err)
	}

	fuseArgs := []string{"-mount", f.Mount, "-backend", f.BackendPath(), "-acls", aclPath}
	if f.Backend.IsRemote() {
		blob, err := f.BackendConfigJSON()
		if err != nil {
			return fmt.Errorf("backend %q config (%s): %w", f.Backend, f.Mount, err)
		}
		fuseArgs = append(fuseArgs,
			"-remote", string(f.Backend),
			"-remote-config", string(blob),
			"-mark", fmt.Sprintf("%d", m.soMark),
		)
	}

	cmd, err := startSidecar(m.ctx, m.children, "sbxfuse:"+f.Slug(), m.fuseBin, fuseArgs, nil,
		sidecarOnEvent(formatFuseEvent, newFuseTranslator(m.broker, f.Mount, gen.Backend(f.Backend)).handle))
	if err != nil {
		return fmt.Errorf("start fuse (%s): %w", f.Mount, err)
	}
	if err := waitForMountReady(m.ctx, f.Mount, mountReadTimout); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("fuse did not mount %s: %w", f.Mount, err)
	}

	// Internal mounts stay host-side only: the sbxfuse daemon is mounted on the
	// sandbox host (so sandboxd can read/write it — e.g. as a snapshot target)
	// but it is never exported into the agent workload, so the agent never sees
	// it. Everything else is exported into the workload's mount namespace.
	if !f.Internal {
		m.isoMu.Lock()
		err = m.iso.ExportWorkspace(m.ctx, f.Mount)
		m.isoMu.Unlock()
		if err != nil {
			_ = cmd.Process.Kill()
			return fmt.Errorf("export workspace %s: %w", f.Mount, err)
		}
	}

	m.mu.Lock()
	m.live[f.Mount] = fsSidecar{pid: cmd.Process.Pid, aclPath: aclPath}
	m.mu.Unlock()
	return nil
}

// stop tears down the sbxfuse daemon for mount and reverses its export. SIGTERM
// gives sbxfuse a chance to fusermount -u (and drain a remote oplog) before the
// WaitDelay kill. A no-op for an unknown mount.
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
	if sc.pid > 0 {
		if err := syscall.Kill(sc.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			log.Printf("sandboxd: reconcile: SIGTERM sbxfuse (mount=%s pid=%d): %v", mount, sc.pid, err)
		}
	}
	m.isoMu.Lock()
	err := m.iso.UnexportWorkspace(m.ctx, mount)
	m.isoMu.Unlock()
	if err != nil {
		log.Printf("sandboxd: reconcile: unexport %s: %v", mount, err)
	}
	_ = os.Remove(sc.aclPath)
}

// reACL rewrites a kept mount's ACL file and SIGHUPs its sbxfuse to reload it.
func (m *mountManager) reACL(f spec.FS) error {
	m.mu.Lock()
	sc, ok := m.live[f.Mount]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if err := writeACLs(sc.aclPath, defaultedACLs(f)); err != nil {
		return fmt.Errorf("reconcile acls (%s): %w", f.Mount, err)
	}
	if err := syscall.Kill(sc.pid, syscall.SIGHUP); err != nil {
		return fmt.Errorf("SIGHUP sbxfuse (mount=%s pid=%d): %w", f.Mount, sc.pid, err)
	}
	return nil
}
