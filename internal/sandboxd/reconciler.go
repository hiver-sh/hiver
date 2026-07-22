package sandboxd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/snapshot"
	"github.com/hiver-sh/hiver/internal/spec"
)

// reconciler.go holds the config→sandbox-state reconciliation: it drives the
// mount manager's per-mount operations from a desired spec and restores the
// spec's snapshot. The logic isn't mount-specific — it's the single place a
// config (at boot or via a later PUT /v1/config) is turned into running state.

// specFromConfig converts a SandboxConfig to a spec.Spec by round-tripping
// through the wire format (their JSON shapes align). Used by createPacked to
// assemble each sandbox's workload spec from its applied config, and by the
// reconcile path to drive the mount manager (which works in spec.FS — it carries
// the backend helpers: BackendPath, Slug, ACL defaults).
func specFromConfig(cfg gen.SandboxConfig) (*spec.Spec, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	var sp spec.Spec
	if err := json.Unmarshal(data, &sp); err != nil {
		return nil, fmt.Errorf("parse config as spec: %w", err)
	}
	// mitm has no wire-format counterpart on proxy.EgressRule (it's a
	// sandbox-level toggle, not a per-rule one) — apply it here as the one
	// place a config becomes the egress rules sbxproxy receives. false
	// forces every rule's Passthrough, the existing per-rule mechanism that
	// makes handleTransparentTLS skip TLS interception and raw-forward the
	// byte stream after a host-only allow decision (see
	// internal/proxy/transparent_tls_linux.go).
	if cfg.Mitm != nil && !*cfg.Mitm {
		for i := range sp.Egress {
			sp.Egress[i].Passthrough = true
		}
	}
	return &sp, nil
}

// SetRootMounted records that iso.MountRoot has run, so the next Reconcile may
// restore a snapshot into the now-assembled overlay. Called once, after MountRoot.
func (m *mountManager) SetRootMounted() { m.rootMounted.Store(true) }

// SetWorkloadLive records that the agent is running, so a later Reconcile injects
// newly-added workspaces into the live workload. Called once, after the workload
// launches (cold or resume path).
func (m *mountManager) SetWorkloadLive() { m.workloadLive.Store(true) }

// planFsReconcile splits the desired mounts against the live set into the ones to
// add (start), the mount paths to remove (stop), and the ones to keep (re-ACL).
// Pure, so the decision logic is unit-testable without spawning sbxfuse.
func planFsReconcile(live map[string]bool, desired []spec.FS) (add []spec.FS, remove []string, keep []spec.FS) {
	want := make(map[string]bool, len(desired))
	for _, f := range desired {
		want[f.Mount] = true
	}
	for mt := range live {
		if !want[mt] {
			remove = append(remove, mt)
		}
	}
	for _, f := range desired {
		if live[f.Mount] {
			keep = append(keep, f)
		} else {
			add = append(add, f)
		}
	}
	return add, remove, keep
}

// Reconcile drives the live mount set to match the spec: stop mounts no longer
// present, start newly-added ones, and reload ACLs for the rest. Level-triggered
// (it diffs the full desired set against the live set) so a missed or
// out-of-order config event can't leave the set stuck. Once the root filesystem
// is mounted it also restores the spec's snapshot (once, before launch), so a
// prewarm sandbox restores the snapshot named by its first applied config.
// Errors are collected so one bad mount doesn't abort the others.
func (m *mountManager) Reconcile(sp *spec.Spec) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	m.mu.Lock()
	liveSet := make(map[string]bool, len(m.live))
	for mt := range m.live {
		liveSet[mt] = true
	}
	m.mu.Unlock()

	add, remove, keep := planFsReconcile(liveSet, sp.FS)

	var errs []error
	for _, mt := range remove {
		m.stop(mt)
		log.Printf("sandboxd: reconcile: unmounted workspace %s (no longer in config)", mt)
	}
	for _, f := range keep {
		if err := m.reACL(f); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range add {
		if err := m.start(f); err != nil {
			errs = append(errs, fmt.Errorf("mount %s: %w", f.Mount, err))
			continue
		}
		log.Printf("sandboxd: reconcile: mounted new workspace %s", f.Mount)
	}

	m.maybeRestore(sp)

	// If the workload is already running, the mount changes above must be applied
	// to it live: workspaces added here are injected, and workspaces removed above
	// (queued by the backend's UnexportWorkspace) are unmounted in the guest —
	// runc/the guest only act on mounts at launch otherwise. No-op at boot
	// (workloadLive is set after launch) and when nothing changed.
	if m.workloadLive.Load() {
		// Empty state: the workload's environment and entrypoint are fixed at launch
		// (both delivered once, at resume); only the mount add/remove set changes
		// here, which the backend reads from its own queued state.
		if err := m.iso.ApplyResumeState(m.ctx, isolation.ResumeState{}); err != nil {
			errs = append(errs, fmt.Errorf("apply workspace changes live: %w", err))
		}
	}
	return errors.Join(errs...)
}

// maybeRestore restores the snapshot named by the spec, once, after the root is
// mounted and before the workload starts. It runs from Reconcile so a prewarm
// sandbox restores the snapshot named by its first applied config. Restoring is a
// strictly pre-launch, at-most-once operation: the CAS on restored claims the one
// opportunity, so a later config-apply (post-launch) can't re-restore.
func (m *mountManager) maybeRestore(sp *spec.Spec) {
	if !m.rootMounted.Load() {
		return
	}
	if !m.restored.CompareAndSwap(false, true) {
		return
	}
	if sp.Snapshot == nil || sp.Snapshot.Files == nil || sp.Snapshot.Files.Key == "" {
		return
	}
	// When a VM snapshot is being resumed, its overlay already carries the full
	// filesystem state — a pre-boot files restore would extract into the (unused,
	// possibly absent) cold-boot overlay and conflict with the resumed overlay. The
	// files snapshot is the cold-boot path only.
	if m.iso.HasPrewarmSnapshot() {
		log.Printf("sandboxd: snapshot: resuming VM snapshot, skipping files restore for %q", sp.Snapshot.Files.Key)
		return
	}
	f := sp.Snapshot.Files
	// snapshot.files.mount points the tarball at a FUSE drive (e.g. an internal
	// remote-backed mount started just above); otherwise fall back to the
	// host's local snapshot directory. The mount path is agent-visible
	// (e.g. /snapshot-drive) — resolve it to sandboxd's own view of that FUSE
	// mount (keyPrefix/mnt/<slug>), since that's where sandboxd reads/writes.
	dir := m.snapshotDir
	if f.Mount != "" {
		dir = m.hostMountPath(f.Mount)
	}
	if dir == "" {
		return
	}
	src := snapshot.SnapshotPath(dir, f.Key)
	if _, err := os.Stat(src); err != nil {
		log.Printf("sandboxd: snapshot: no snapshot found at %s, starting fresh", src)
		return
	}
	log.Printf("sandboxd: snapshot: restoring %s", src)
	if err := m.iso.RestoreSnapshot(src, f.Include); err != nil {
		log.Fatalf("snapshot restore: %v", err)
	}
}
