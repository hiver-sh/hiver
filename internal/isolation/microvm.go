package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hiver-sh/hiver/internal/firecracker"
	"github.com/hiver-sh/hiver/internal/runc"
	"github.com/hiver-sh/hiver/internal/snapshot"
)

// Guest network: a /30 point-to-point link over the tap device. The host
// owns the gateway address and the guest owns the other usable address; the
// guest routes all egress at the gateway, where the host REDIRECTs it to
// sbxproxy. These are the boot-sandbox defaults; a packed sandbox derives its
// own per-VM address/gateway/MAC from Config.GuestIP (see newMicroVM).
const (
	bootGuestIP   = "172.16.0.2"
	bootGatewayIP = "172.16.0.1"
	bootGuestMAC  = "06:00:ac:10:00:02"
)

// controlAttemptTimeout bounds one ApplyResumeState control RPC so a connection
// that is accepted but never answered (a starved, just-resumed guest) is
// abandoned and retried by the self-heal loop instead of blocking it.
const controlAttemptTimeout = 3 * time.Second

// microvm is the firecracker-backed Isolation. The agent runs inside a
// guest VM with its own kernel, so each primitive targets the guest:
//
//   - filesystem: the image rootfs is attached as a read-only block device
//     and a writable overlay device on top; the guest agent stacks them
//     with overlayfs and mounts the FUSE workspaces (capability 1);
//   - network: a host tap device carries guest egress, which the host
//     REDIRECTs to sbxproxy; the in-guest firewall mirrors the rules
//     (capability 2);
//   - exec: commands are proxied to the in-guest agent over vsock via the
//     sbxvsock bridge (capability 3).
//
// Host-controllable work (building the drives, the tap, the firecracker boot
// config, the vsock channel, and placing the VMM in the sandbox cgroup for
// resource accounting) is implemented here. The guest kernel, rootfs, and the
// in-guest agent (cmd/sbxguest) supply the other half; their paths are
// resolved from the environment at runtime.
type microvm struct {
	hostname string

	// cgroupPath is the absolute cgroup the firecracker VMM is placed in so
	// its (and thus the guest's) CPU/memory are accounted; PollResourceUsage
	// reads /sys/fs/cgroup<cgroupPath>.
	cgroupPath string

	// Host-side artifact paths, all under jailDir.
	jailDir    string
	apiSock    string
	configFile string
	logFile    string
	rootfsImg  string
	overlayImg string
	paramsImg  string

	// baseDir, when non-empty, is the shared per-image base snapshot this VM
	// resumes from (pack mode, design §7): ResumeAgent binds per-VM overlay/vsock
	// over the base's canonical paths and ResumeReady loads base/{snapshot,mem}.bin
	// with a per-VM tap override. Empty for cold boot and the in-place prewarm
	// resume (which reuses this VM's own snapshot files).
	baseDir string

	// Compute allocation for the guest, fixed at boot (SandboxConfig.cpu /
	// .memory). New defaults these before construction.
	vcpuCount  int
	memSizeMib int

	// Per-VM network identity. For the boot sandbox these are the boot*
	// defaults; for a packed sandbox they are derived from Config.GuestIP so N
	// VMs in one pod don't collide. guestIP is baked into the kernel cmdline
	// (DefaultBootArgs); gatewayIP is the host end of the /30 on the tap.
	guestIP   string
	gatewayIP string
	guestMAC  string

	// Toolchain + guest assets, resolved from env with defaults.
	fcBin   string // firecracker binary
	kernel  string // guest kernel (vmlinux)
	tapName string

	// netnsName, when set, is the per-VM network namespace a packed VM's tap and
	// firecracker run in (design §7, "option 1"): every packed VM keeps the base
	// snapshot's baked guest IP (bootGuestIP) in its own netns — so a resident
	// prewarmed workload is never re-IP'd and its sockets/DNS/TLS stay valid — and
	// the host SNATs its egress to sourceIP so sbxproxy still keys per sandbox.
	// Empty for the boot/base VM, which uses the pod netns.
	netnsName string
	// sourceIP is the per-VM host-side identity (172.16.<n>.2) the netns SNATs
	// guest egress to; it is what sbxproxy sees and keys egress rules on. Empty
	// unless packed.
	sourceIP string

	// prealloc marks a packed VM claiming a preallocated slot whose
	// netns/veth/tap/iptables (+DNS sink) and CoW overlay image were provisioned
	// ahead of time by the pod's prealloc pool (Config.Prealloc). RedirectEgress
	// and the overlay build in MountRoot are then no-ops, and UnmountRoot leaves
	// the netns teardown to the pool, which resets the slot and returns it.
	prealloc bool

	// localMounts are the host-side local-backend dirs (for snapshot).
	localMounts []SnapshotMount

	// Accumulated state from the capability calls, consumed by LaunchAgent.
	mu   sync.Mutex
	fuse []firecracker.GuestFuse
	// fuseCancel stops each mount's 9p server (UnexportWorkspace).
	fuseCancel map[string]context.CancelFunc
	// fuseHost/fuseCtx record each export's host mountpoint and parent context so
	// bindWorkspaceListeners can start its in-netns 9p listener once the packed VM's
	// netns exists (the export is recorded during the pre-netns workspace reconcile).
	fuseHost map[string]string
	fuseCtx  map[string]context.Context
	// pendingUnmount holds guest workspace paths removed by UnexportWorkspace that
	// the guest hasn't been told to umount yet. The next ApplyResumeState carries
	// them in the control RPC (UnmountWorkspaces) and clears them on success — the
	// host-side 9p teardown alone leaves the guest mountpoint behind.
	pendingUnmount []string
	// nextFusePort hands out vsock ports monotonically so a mount removed at
	// runtime doesn't free a port a later add would reuse while the guest may
	// still reference it.
	nextFusePort uint32
	proxyPort    int
	mark         int
	caCertPEM    []byte
	// nssDB is the pre-built NSS database (cert9.db/key9.db/pkcs11.txt keyed by
	// base name) trusting the sandbox CA, built host-side in InstallCA and handed
	// to the guest via the params drive so Chromium/Playwright trust sbxproxy.
	nssDB map[string][]byte

	// Prewarm snapshot-resume state. snapFile/memFile hold the full VM snapshot
	// PrewarmSnapshot writes (device/vCPU state and guest memory respectively);
	// haveSnapshot gates the resume path. All under jailDir, so UnmountRoot's
	// RemoveAll reclaims them.
	snapFile     string
	memFile      string
	haveSnapshot bool
}

func newMicroVM(cfg Config) *microvm {
	// The boot sandbox (no GuestIP) keeps the historical single-tenant layout:
	// jail/cgroup/tap keyed by pod hostname and the fixed boot* network identity.
	// A packed sandbox (GuestIP set) gets a per-VM identity derived from its IP so
	// N VMs coexist in one pod: jail/tap keyed by the IP index (short, keeps the
	// vsock AF_UNIX path well under 108 bytes), cgroup namespaced by Key, and its
	// own /30 (gateway = .1 of the link) + MAC.
	id := cfg.Hostname
	cgroupName := cfg.Hostname
	// Every microvm — boot, base, and packed — keeps the base snapshot's baked
	// guest identity (bootGuestIP/bootGatewayIP/bootGuestMAC). Packed VMs no longer
	// re-IP to a per-VM address (that broke prewarmed resident workloads bound to
	// the baked IP); instead each runs in its own netns and the host SNATs it to
	// cfg.GuestIP (172.16.<n>.2) for sbxproxy's per-source keying.
	gIP, gwIP, mac := bootGuestIP, bootGatewayIP, bootGuestMAC
	tap := tapNameFor(cfg.Hostname)
	var netnsName, sourceIP string
	if cfg.GuestIP != "" {
		n := netID(cfg.GuestIP)
		id = n
		sourceIP = cfg.GuestIP
		netnsName = "fcsbx" + n
		tap = "fctap-" + n
	}
	prealloc := cfg.GuestIP != "" && cfg.Prealloc
	if cfg.Key != "" {
		cgroupName = cfg.Hostname + "-" + cfg.Key
	}
	jail := filepath.Join(envOr("FIRECRACKER_RUN_DIR", "/run/firecracker"), id)
	m := &microvm{
		hostname:    cfg.Hostname,
		cgroupPath:  sandboxCgroupPath(cgroupName),
		jailDir:     jail,
		apiSock:     filepath.Join(jail, "firecracker.sock"),
		configFile:  filepath.Join(jail, "config.json"),
		logFile:     filepath.Join(jail, "firecracker.log"),
		rootfsImg:   filepath.Join(jail, "rootfs.ext4"),
		overlayImg:  filepath.Join(jail, baseOverlayName),
		paramsImg:   filepath.Join(jail, "metadata.ext4"),
		snapFile:    filepath.Join(jail, baseSnapshotName),
		memFile:     filepath.Join(jail, baseMemName),
		baseDir:     cfg.BaseSnapshotDir,
		guestIP:     gIP,
		gatewayIP:   gwIP,
		guestMAC:    mac,
		vcpuCount:   cfg.VcpuCount,
		memSizeMib:  cfg.MemoryMiB,
		fcBin:       envOr("FIRECRACKER_BIN", "firecracker"),
		kernel:      envOr("FIRECRACKER_KERNEL", "/var/lib/firecracker/vmlinux"),
		tapName:     tap,
		netnsName:   netnsName,
		sourceIP:    sourceIP,
		prealloc:    prealloc,
		localMounts: cfg.LocalMounts,
	}
	// A packed sandbox with a ready base snapshot takes the resume fast path:
	// HasPrewarmSnapshot reports true so the caller resumes instead of cold-booting.
	if m.baseDir != "" && baseSnapshotReady(m.baseDir) {
		m.haveSnapshot = true
	}
	return m
}

// Canonical artifact names shared by a VM's jail and the pack-mode base snapshot.
// Using one set of names means the base builder's jail layout already matches what
// a resumer binds over, so no path rewriting is needed.
const (
	baseSnapshotName = "snapshot.bin"
	baseMemName      = "mem.bin"
	baseOverlayName  = "overlay.ext4"
)

// baseSnapshotReady reports whether a base dir holds a complete snapshot triple
// (state + memory + the warm overlay each VM copies-on-write from).
func baseSnapshotReady(dir string) bool {
	for _, f := range []string{baseSnapshotName, baseMemName, baseOverlayName} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Size() == 0 {
			return false
		}
	}
	return true
}

// baseBuilderID is the jail id (hence dir/tap/cgroup name) of the transient VM
// that builds a pod's shared base snapshot. It must not collide with any packed
// sandbox: those are keyed by their guest-IP index (172.16.<n>.2, n≥2), so the
// builder's hostname-derived id and its 172.16.0.x boot network are disjoint.
const baseBuilderID = "base"

// MicroVMBaseDir is the shared dir a pod's base snapshot lives in — the transient
// builder's jail. Both the builder (which writes it) and every resumer (which
// binds its canonical overlay/vsock paths) resolve it the same way.
func MicroVMBaseDir() string {
	return filepath.Join(envOr("FIRECRACKER_RUN_DIR", "/run/firecracker"), baseBuilderID)
}

// BuildMicroVMBaseSnapshot boots a transient firecracker guest running the image
// prewarm entrypoint with the pod's shared sidecars (proxy/DNS), snapshots it into
// MicroVMBaseDir, and tears the guest down. Packed sandboxes then resume from this
// base (Config.BaseSnapshotDir) instead of cold-booting (design §7). vcpu/memMiB
// size the base guest; every resumer inherits that sizing (firecracker fixes
// vCPU/RAM in the snapshot). Returns the base dir on success; on failure the
// caller cold-boots each VM. Safe to call once per pod.
func BuildMicroVMBaseSnapshot(ctx context.Context, certPEM []byte, imgCfg *runc.ImageConfig, vcpu, memMiB, proxyPort, dnsPort, mark int) (string, error) {
	baseDir := MicroVMBaseDir()
	_ = os.RemoveAll(baseDir) // discard any stale base from a prior incarnation
	b := newMicroVM(Config{Hostname: baseBuilderID, VcpuCount: vcpu, MemoryMiB: memMiB})
	if err := b.MountRoot(); err != nil {
		return "", fmt.Errorf("base: mount root: %w", err)
	}
	if len(certPEM) > 0 {
		if err := b.InstallCA(certPEM); err != nil {
			return "", fmt.Errorf("base: install CA: %w", err)
		}
	}
	// Wire the builder's egress to the shared sidecars so the prewarm workload can
	// reach the network as it warms up; the rules key on the builder's own tap.
	if err := b.RedirectEgress(ctx, proxyPort, dnsPort, mark); err != nil {
		_ = exec.Command("ip", "link", "del", b.tapName).Run()
		return "", fmt.Errorf("base: egress: %w", err)
	}
	if err := b.PrewarmSnapshot(ctx, imgCfg); err != nil {
		_ = exec.Command("ip", "link", "del", b.tapName).Run()
		return "", fmt.Errorf("base: prewarm snapshot: %w", err)
	}
	// The transient VM is stopped; drop its tap (the snapshot/mem/overlay remain in
	// baseDir, and the vsock dir stays as the canonical bind target for resumers).
	_ = exec.Command("ip", "link", "del", b.tapName).Run()
	if !baseSnapshotReady(baseDir) {
		return "", fmt.Errorf("base: snapshot incomplete in %s", baseDir)
	}
	return baseDir, nil
}

// gatewayForGuest returns the host (gateway) end of a packed sandbox's /30 link:
// the guest is 172.16.<n>.2, the gateway 172.16.<n>.1. Falls back to the input
// for a malformed address (callers only pass allocator-formed IPs).
func gatewayForGuest(guestIP string) string {
	parts := strings.Split(guestIP, ".")
	if len(parts) != 4 {
		return guestIP
	}
	parts[3] = "1"
	return strings.Join(parts, ".")
}

// macForGuest derives a stable, locally-administered MAC from a packed
// sandbox's guest IP (06:00:ac:10:<n>:<host>), so each VM's eth0 differs. Each
// tap is its own L2 segment, so this is belt-and-suspenders rather than required
// for correctness. Malformed input falls back to the boot MAC.
func macForGuest(guestIP string) string {
	parts := strings.Split(guestIP, ".")
	if len(parts) != 4 {
		return bootGuestMAC
	}
	n, _ := strconv.Atoi(parts[2])
	host, _ := strconv.Atoi(parts[3])
	return fmt.Sprintf("06:00:ac:10:%02x:%02x", n&0xff, host&0xff)
}

func (m *microvm) Kind() Kind { return KindMicroVM }

// bundledRootfsImg is the read-only rootfs ext4 the bundler pre-builds from the
// agent rootfs at image-build time (docker/bundler.Dockerfile). MountRoot
// attaches it directly instead of running mke2fs -d on every boot — it's
// identical across every sandbox of this image and mounted read-only, so all
// guests safely share the one baked file. It already carries the guest init
// (/usr/bin/sbxguest, matching init= in the boot args); sbxfuse is not in here
// (it runs host-side, reached over 9p-over-vsock).
const bundledRootfsImg = runc.MntDir + "/rootfs.ext4"

// MountRoot materialises the two block devices the guest stacks into its root:
// the pre-built read-only rootfs image (bundledRootfsImg, the lower) and a
// freshly-created empty overlay.ext4 (the writable upper). The guest agent
// assembles the overlay; the host only points the root drive at the baked image
// (shared, outside jailDir, so UnmountRoot's RemoveAll leaves it intact) and
// builds the per-sandbox overlay.
func (m *microvm) MountRoot() error {
	if err := os.MkdirAll(m.jailDir, 0o755); err != nil {
		return fmt.Errorf("create jail dir: %w", err)
	}
	fi, err := os.Stat(bundledRootfsImg)
	if err != nil {
		return fmt.Errorf("stat bundled rootfs image: %w", err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("bundled rootfs image %s is empty", bundledRootfsImg)
	}
	m.rootfsImg = bundledRootfsImg
	// Prealloc: the slot's CoW overlay image was built (empty) ahead of time by the
	// pool and reset on its last release, so skip the rebuild on the claim path.
	if m.prealloc {
		if fi, err := os.Stat(m.overlayImg); err == nil && fi.Size() > 0 {
			return nil
		}
	}
	if err := buildEmptyExt4(m.overlayImg, 2048); err != nil { // 2 GiB writable upper
		return fmt.Errorf("build overlay image: %w", err)
	}
	return nil
}

func (m *microvm) UnmountRoot() error {
	// Best-effort teardown of the per-sandbox artifacts and network. A packed VM's
	// tap lives in its netns, so deleting the netns drops it (and the veth/rules);
	// the boot/base VM's tap is in the pod netns and is deleted directly.
	// A prealloc slot's netns (and the tap inside it) is owned by the pod's
	// prealloc pool, which resets and reuses it; tearing it down here would race
	// that. The jailDir (incl. the dirty CoW overlay) is still removed — the pool
	// rebuilds an empty overlay when it resets the slot.
	if m.netnsName != "" {
		if !m.prealloc {
			m.teardownPackedNetMicrovm(context.Background())
		}
	} else {
		_ = exec.Command("ip", "link", "del", m.tapName).Run()
	}
	return os.RemoveAll(m.jailDir)
}

// ExportWorkspace records a workspace mount (host sbxfuse dir, guest mount path,
// assigned 9p TCP port) and, where the netns is already up, starts its host-side
// 9p listener. Every guest workspace op then lands on the host FUSE daemon,
// reusing its ACL enforcement, audit events, and remote-backend handling.
func (m *microvm) ExportWorkspace(ctx context.Context, hostMount, guestMount string) error {
	// hostMount is the host-side sbxfuse dir the 9p server serves; guestMount is
	// where the guest mounts it (and the key UnexportWorkspace looks up by, via the
	// mountManager). For the boot sandbox the two coincide, but a packed sandbox's
	// host path is per-key (/run/sandboxd/<key>/mnt/...) while its guest path is the
	// configured mount (e.g. /workspace) — so the guest must be told to mount at
	// guestMount, not the host path, or /workspace never appears in the guest.
	m.mu.Lock()
	if m.nextFusePort == 0 {
		m.nextFusePort = firecracker.GuestFuseBasePort
	}
	port := m.nextFusePort
	m.nextFusePort++
	m.fuse = append(m.fuse, firecracker.GuestFuse{Mount: guestMount, Port: port})
	// Record host mount + ctx so bindWorkspaceListeners can start this listener
	// after a snapshot resume (Firecracker wipes the socket on /snapshot/load).
	if m.fuseHost == nil {
		m.fuseHost = map[string]string{}
		m.fuseCtx = map[string]context.Context{}
	}
	m.fuseHost[guestMount] = hostMount
	m.fuseCtx[guestMount] = ctx
	m.mu.Unlock()

	// A packed VM's per-key netns is created *after* the workspace reconcile (egress
	// setup runs later), and the guest is told to mount only after the resume — so
	// defer binding the 9p listener to bindWorkspaceListeners, called from
	// ApplyResumeState once the netns and guest are up. The boot/non-packed VM has
	// its tap in the pod netns already, so bind immediately.
	if m.netnsName != "" {
		return nil
	}
	return m.serveFuse(ctx, hostMount, guestMount, port)
}

// bindWorkspaceListeners starts the host-side 9p listener for every recorded
// workspace export that isn't already serving. Called from ApplyResumeState
// (packed resume) once the VM's netns exists, so the guest's mount dial to the
// netns gateway reaches a live listener. Idempotent.
func (m *microvm) bindWorkspaceListeners() error {
	m.mu.Lock()
	type todo struct {
		host, guest string
		port        uint32
		ctx         context.Context
	}
	var pending []todo
	for _, f := range m.fuse {
		if f.Port == 0 || (m.fuseCancel != nil && m.fuseCancel[f.Mount] != nil) {
			continue // not exportable, or already serving
		}
		pending = append(pending, todo{m.fuseHost[f.Mount], f.Mount, f.Port, m.fuseCtx[f.Mount]})
	}
	m.mu.Unlock()
	for _, p := range pending {
		ctx := p.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := m.serveFuse(ctx, p.host, p.guest, p.port); err != nil {
			return fmt.Errorf("bind 9p listener %s: %w", p.guest, err)
		}
	}
	return nil
}

// serveFuse binds the host-side 9p listener for one workspace (the guest dials the
// netns gateway on `port`; served from hostMount) and starts its server. The
// listener lives inside the VM's netns (or the pod netns for a boot VM), so it
// survives a snapshot resume — unlike the former vsock socket. Each mount runs on
// its own context so UnexportWorkspace can stop just that one.
func (m *microvm) serveFuse(ctx context.Context, hostMount, guestMount string, port uint32) error {
	srvCtx, cancel := context.WithCancel(ctx)
	ln, err := listenTCPInNetns(m.netnsName, fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		cancel()
		return err
	}
	m.mu.Lock()
	if m.fuseCancel == nil {
		m.fuseCancel = map[string]context.CancelFunc{}
	}
	m.fuseCancel[guestMount] = cancel
	m.mu.Unlock()
	go func() {
		if err := firecracker.Serve9P(srvCtx, hostMount, ln); err != nil && srvCtx.Err() == nil {
			// Logged via the server's own path; nothing actionable here.
			_ = err
		}
	}()
	return nil
}

// UnexportWorkspace stops the mount's 9p server and drops it from the guest
// fuse table. Best-effort: unknown mounts are ignored.
func (m *microvm) UnexportWorkspace(ctx context.Context, mount string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.fuseCancel[mount]; ok {
		cancel()
		delete(m.fuseCancel, mount)
	}
	delete(m.fuseHost, mount)
	delete(m.fuseCtx, mount)
	removed := false
	for i := range m.fuse {
		if m.fuse[i].Mount == mount {
			m.fuse = append(m.fuse[:i], m.fuse[i+1:]...)
			removed = true
			break
		}
	}
	// Queue the guest-side umount: stopping the host 9p server above frees the
	// export but leaves the mountpoint in the guest. The next ApplyResumeState
	// (issued by the reconcile after this stop) tells the guest to umount it.
	if removed {
		m.pendingUnmount = append(m.pendingUnmount, mount)
	}
	return nil
}

// clearPendingUnmount drops the given paths from pendingUnmount once the guest
// has acked their umount, so a later control RPC doesn't redundantly resend them.
// A concurrently-queued path (not in done) is preserved.
func (m *microvm) clearPendingUnmount(done []string) {
	if len(done) == 0 {
		return
	}
	cleared := make(map[string]bool, len(done))
	for _, p := range done {
		cleared[p] = true
	}
	m.mu.Lock()
	kept := m.pendingUnmount[:0]
	for _, p := range m.pendingUnmount {
		if !cleared[p] {
			kept = append(kept, p)
		}
	}
	m.pendingUnmount = kept
	m.mu.Unlock()
}

// InstallCA stashes the sandbox CA so LaunchAgent embeds it in the params
// drive; the guest agent splices it into the workload trust store at boot. It
// also builds the NSS database host-side (certutil ships in the core image, the
// guest has no NSS tooling) so the guest can drop it into $HOME/.pki/nssdb for
// Chromium/Playwright. NSS build failures are non-fatal — they only affect NSS
// clients, and most images run none.
func (m *microvm) InstallCA(certPEM []byte) error {
	nssDB, err := buildNSSDB(certPEM)
	if err != nil {
		log.Printf("install NSS CA: %v (NSS clients won't trust the sandbox CA)", err)
	}
	m.mu.Lock()
	m.caCertPEM = append([]byte(nil), certPEM...)
	m.nssDB = nssDB
	m.mu.Unlock()
	return nil
}

// RedirectEgress provisions the host tap device that carries guest egress
// and installs the nat rules that funnel guest TCP to sbxproxy and guest DNS
// to the sinkhole. The guest reaches the host at gatewayIP; the host receives
// the forwarded packets in PREROUTING (not OUTPUT — the guest is a separate
// network stack) and DNATs them to the sidecars on loopback.
//
// DNAT to 127.0.0.1, not REDIRECT: sbxproxy listens on 127.0.0.1:proxyPort
// (and the sink on 127.0.0.1:dnsPort), but REDIRECT on a *forwarded* packet
// rewrites the destination to the incoming interface's primary address
// (gatewayIP), where nothing is listening — so the redirect would silently
// black-hole guest egress. DNAT'ing straight to 127.0.0.1 lands it on the
// sidecar; route_localnet (set below) lifts the kernel's martian-drop of
// loopback-destined packets arriving on the tap, and SO_ORIGINAL_DST still
// recovers the guest's real destination from conntrack.
//
// The guest's resolv.conf points its nameserver at gatewayIP (see
// resolvConfForGuest); both UDP/53 and TCP/53 to it are DNAT'd to the sink, so
// no in-pod resolver listens on the gateway and DNS can't leave the box.
func (m *microvm) RedirectEgress(ctx context.Context, proxyPort, dnsPort, mark int) error {
	m.mu.Lock()
	m.proxyPort = proxyPort
	m.mark = mark
	m.mu.Unlock()

	// Packed VM: run the tap + firecracker in a per-VM netns and SNAT to sourceIP
	// (design §7, "option 1"). The guest keeps the base snapshot's baked IP, so it
	// is never re-IP'd. The boot/base VM (no netns) keeps the pod-netns tap below.
	if m.netnsName != "" {
		// Prealloc: the pod's prealloc pool already wired this octet's
		// netns/veth/tap/iptables and started its DNS sink, so the claim path has
		// nothing to do — the slot is reused as-is (the pool flushes per-tenant
		// residue on release).
		if m.prealloc {
			return nil
		}
		return m.setupPackedNetMicrovm(ctx, proxyPort, dnsPort, mark)
	}

	steps := [][]string{
		{"ip", "tuntap", "add", "dev", m.tapName, "mode", "tap"},
		{"ip", "addr", "add", m.gatewayIP + "/30", "dev", m.tapName},
		{"ip", "link", "set", "dev", m.tapName, "up"},
		// Guest DNS (UDP and TCP/53) is DNAT'd to the host DNS sinkhole. These
		// must precede the general TCP rule so DNS doesn't fall through to the
		// proxy.
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", m.tapName, "-p", "udp", "--dport", guestDNSPort, "-j", "DNAT", "--to-destination", fmt.Sprintf("127.0.0.1:%d", dnsPort)},
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", m.tapName, "-p", "tcp", "--dport", guestDNSPort, "-j", "DNAT", "--to-destination", fmt.Sprintf("127.0.0.1:%d", dnsPort)},
		// All other guest TCP arriving on the tap is DNAT'd to the host proxy.
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", m.tapName, "-p", "tcp", "-j", "DNAT", "--to-destination", fmt.Sprintf("127.0.0.1:%d", proxyPort)},
		// Proxy-originated upstream traffic (stamped with SO_MARK) escapes
		// any redirect so it isn't looped back.
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-m", "mark", "--mark", fmt.Sprintf("0x%x", mark), "-j", "RETURN"},
		// Block all non-TCP guest egress. Guest TCP and DNS were DNAT'd to the
		// host proxy/sink in PREROUTING above (delivered locally, never
		// forwarded), so the only traffic reaching the FORWARD chain off the tap
		// is the workload trying to egress non-TCP directly — UDP, ICMP, SCTP,
		// raw IP (the workload holds CAP_NET_RAW). Drop it. The ! -p tcp guard is
		// belt-and-suspenders — no guest TCP reaches FORWARD here because the
		// PREROUTING DNAT catches all of it — and it closes the ICMP/raw-socket
		// channels alongside UDP.
		{"iptables", "-t", "filter", "-A", "FORWARD", "-i", m.tapName, "!", "-p", "tcp", "-j", "DROP"},
	}
	if err := enableIPForward(); err != nil {
		return err
	}
	for _, s := range steps {
		if out, err := exec.CommandContext(ctx, s[0], s[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w (%s)", s, err, out)
		}
	}
	// Drop guest IPv6 egress. The v4 DNAT/proxy path has no v6 equivalent, so
	// any guest v6 forwarded off the tap would bypass the proxy; drop it the
	// same way the v4 non-TCP FORWARD rule does.
	if err := dropIPv6Egress(ctx, [][]string{
		{"-A", "FORWARD", "-i", m.tapName, "-j", "DROP"},
	}); err != nil {
		return err
	}
	// route_localnet lets the kernel deliver the DNAT-to-127.0.0.1 packets
	// instead of dropping them as martians.
	if err := enableRouteLocalnet(); err != nil {
		return err
	}
	return nil
}

func (m *microvm) CgroupPath() string { return m.cgroupPath }

// CaptureSnapshot writes the same gzip-tar snapshot format the container
// backend produces, honouring per-path include. It loop-mounts the overlay
// drive image read-only and runs the shared snapshot package against the
// guest's writable layer, so a microvm snapshot is byte-compatible with a
// container one. Runs after the guest has powered off (the image is then
// quiescent); requires the CAP_SYS_ADMIN sandboxd already uses for overlays.
func (m *microvm) CaptureSnapshot(dst string, include []string) error {
	return m.withOverlayMount(true, func(upper string, mounts []snapshot.MountSource) error {
		return snapshot.Capture(dst, upper, mounts, include)
	})
}

// RestoreSnapshot extracts a captured snapshot into the freshly built overlay
// drive before boot, so the guest comes up on the prior writable state. Must
// run after MountRoot (which created the empty overlay) and before LaunchAgent.
func (m *microvm) RestoreSnapshot(src string) error {
	return m.withOverlayMount(false, func(upper string, mounts []snapshot.MountSource) error {
		return snapshot.Restore(src, upper, mounts)
	})
}

// withOverlayMount loop-mounts the overlay drive image at a temp dir and
// invokes fn with the guest's overlay upper dir and the local-FUSE mount
// sources resolved inside it (matching how the guest lays them out under the
// overlay root). The mount is always unmounted and the temp dir removed.
func (m *microvm) withOverlayMount(readonly bool, fn func(upper string, mounts []snapshot.MountSource) error) error {
	mp, err := os.MkdirTemp("", "sbx-snap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mp)

	// The sandbox container's /dev has no loop nodes (it is a tmpfs, not
	// devtmpfs, so the kernel never populates them). Create loop-control and a
	// pool of loop devices so `mount -o loop` can allocate one; the controller
	// grants the matching device-cgroup rules for microvm sandboxes.
	if err := ensureLoopNodes(); err != nil {
		return fmt.Errorf("provision loop devices: %w", err)
	}

	// Always loop-mount read-write, even for capture: the guest powers off
	// without cleanly unmounting its ext4 overlay, so the journal is dirty and
	// a read-only mount is refused ("cannot mount ... read-only") because the
	// journal cannot be replayed. The image is quiescent here (VM stopped), so
	// a read-write mount safely recovers it; capture only reads from it.
	if out, err := exec.Command("mount", "-o", "loop", m.overlayImg, mp).CombinedOutput(); err != nil {
		return fmt.Errorf("mount overlay image: %w (%s)", err, out)
	}
	defer exec.Command("umount", mp).Run()

	// The agent's non-workspace root writes live in the overlay image's
	// upper dir; workspace (local-backend) data lives in the host backend
	// dirs (sbxfuse is host-side), captured via the localMounts table.
	upper := filepath.Join(mp, "upper")
	if !readonly {
		if err := os.MkdirAll(upper, 0o755); err != nil {
			return err
		}
	}
	mounts := make([]snapshot.MountSource, 0, len(m.localMounts))
	for _, lm := range m.localMounts {
		mounts = append(mounts, snapshot.MountSource{ContainerPath: lm.ContainerPath, HostDir: lm.HostDir})
	}
	return fn(upper, mounts)
}

// loopDevicePoolSize is how many /dev/loopN nodes ensureLoopNodes creates.
// `mount -o loop` allocates the first free one via /dev/loop-control; a small
// pool covers concurrent snapshot mounts within a sandbox.
const loopDevicePoolSize = 8

// ensureLoopNodes creates the loop-control char device and a pool of loop
// block devices in the container's /dev when they are missing. The host kernel
// owns the loop subsystem; these are just the device nodes `mount -o loop`
// needs to find, which a container tmpfs /dev lacks. Requires CAP_MKNOD and the
// device-cgroup rules the controller grants for microvm sandboxes.
func ensureLoopNodes() error {
	// loop-control: char 10:237; loopN: block 7:N (see <linux/major.h>).
	if err := mknodIfMissing("/dev/loop-control", unix.S_IFCHR|0o660, 10, 237); err != nil {
		return err
	}
	for i := 0; i < loopDevicePoolSize; i++ {
		if err := mknodIfMissing(fmt.Sprintf("/dev/loop%d", i), unix.S_IFBLK|0o660, 7, i); err != nil {
			return err
		}
	}
	return nil
}

// mknodIfMissing creates a device node, treating an already-existing node as
// success (the host may have populated it, e.g. when /dev is devtmpfs).
func mknodIfMissing(path string, mode uint32, major, minor int) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := unix.Mknod(path, mode, int(unix.Mkdev(uint32(major), uint32(minor)))); err != nil && !os.IsExist(err) {
		return fmt.Errorf("mknod %s: %w", path, err)
	}
	return nil
}

// LaunchAgent writes the metadata drive + firecracker boot config from the
// accumulated capability state and returns the command that boots the VM.
func (m *microvm) LaunchAgent(cfg AgentConfig) (string, []string, error) {
	// /etc/hosts from the pod is handed to the guest so name resolution matches
	// a shared-netns container (which bind-mounts it). Best-effort: a missing
	// file just yields empty content. resolv.conf is rewritten to point at the
	// tap gateway, where the in-pod DNS relay (RedirectEgress) listens — the
	// guest can't reach the pod's loopback resolver directly.
	etcHosts, _ := os.ReadFile("/etc/hosts")
	podResolv, _ := os.ReadFile("/etc/resolv.conf")
	etcResolv := resolvConfForGuest(podResolv, m.gatewayIP)

	m.mu.Lock()
	params := firecracker.GuestParams{
		Entrypoint:     cfg.ImageConfig.Entrypoint,
		Cmd:            cfg.ImageConfig.Cmd,
		Env:            envSlice(cfg.ImageConfig.Env, cfg.Env),
		WorkingDir:     cfg.ImageConfig.WorkingDir,
		Prewarm:        cfg.Prewarm,
		Fuse:           m.fuse,
		ProxyPort:      m.proxyPort,
		Mark:           m.mark,
		ProxyAddr:      fmt.Sprintf("%s:%d", m.gatewayIP, m.proxyPort),
		CACertPEM:      m.caCertPEM,
		EtcHosts:       etcHosts,
		EtcResolvConf:  etcResolv,
		NodeCACertPath: NodeCACertPath,
		NSSDB:          m.nssDB,
	}
	m.mu.Unlock()

	if err := m.buildParamsDrive(params); err != nil {
		return "", nil, fmt.Errorf("build metadata drive: %w", err)
	}

	// Firecracker opens the log file for appending rather than creating it.
	if err := os.WriteFile(m.logFile, nil, 0o644); err != nil {
		return "", nil, fmt.Errorf("create log file: %w", err)
	}

	fcCfg := firecracker.Config{
		BootSource: firecracker.BootSource{
			KernelImagePath: m.kernel,
			BootArgs:        firecracker.DefaultBootArgs(m.guestIP, m.gatewayIP),
		},
		MachineConfig: firecracker.MachineConfig{
			VcpuCount:  m.vcpuCount,
			MemSizeMib: m.memSizeMib,
			Smt:        false,
		},
		Drives: []firecracker.Drive{
			{DriveID: firecracker.RootDriveID, PathOnHost: m.rootfsImg, IsRootDevice: true, IsReadOnly: true},
			{DriveID: firecracker.OverlayDriveID, PathOnHost: m.overlayImg, IsRootDevice: false, IsReadOnly: false},
			{DriveID: firecracker.MetadataDriveID, PathOnHost: m.paramsImg, IsRootDevice: false, IsReadOnly: true},
		},
		NetworkInterfaces: []firecracker.NetworkInterface{
			{IfaceID: "eth0", HostDevName: m.tapName, GuestMAC: m.guestMAC},
		},
		Logger: &firecracker.Logger{LogPath: m.logFile, Level: "Info"},
	}
	if err := firecracker.WriteConfigFile(m.configFile, fcCfg); err != nil {
		return "", nil, fmt.Errorf("write firecracker config: %w", err)
	}

	m.applyCgroupLimits()
	bin, args := firecracker.Command(m.fcBin, m.apiSock, m.configFile)
	wbin, wargs := m.cgroupWrap(bin, args)
	return wbin, wargs, nil
}

// cgroupWrap wraps a firecracker invocation in an `sh -c` that places the
// process in the sandbox cgroup before exec'ing it, so its CPU/memory (the
// guest runs as VMM threads) are accounted under CgroupPath while the PID —
// already in the cgroup — survives the exec as the supervised agent process.
//
// firecracker's stdout/stderr only ever carry the VMM banner ("Running
// Firecracker ...") and the few early guest-kernel boot lines the serial console
// emits before the cmdline disables it — all real workload I/O (exec, file ops,
// TTYs) runs over vsock. That boot noise would otherwise surface on the
// published sandbox log stream, so drop it. Gated by the same
// FIRECRACKER_DEBUG_CONSOLE toggle as the serial console (DefaultBootArgs) so it
// stays visible when you need to watch a boot failure.
func (m *microvm) cgroupWrap(bin string, args []string) (string, []string) {
	cgDir := filepath.Join("/sys/fs/cgroup", m.cgroupPath)
	// firecracker MUST inherit the supervisor's stdio pipes — do not redirect them
	// to /dev/null. superviseStdio derives the agent-exit signal (agentDone) from
	// those pipes reaching EOF; a `>/dev/null` redirect closes them at exec time, so
	// agentDone fires immediately and the teardown goroutine SIGTERMs the VM ~200ms
	// after a resume (the silent "dies shortly after resume" bug — masked only by
	// FIRECRACKER_DEBUG_CONSOLE, which happens to also skip this redirect). In
	// non-console mode firecracker emits ~nothing here; the supervisor drains and
	// discards it (onStdio is nil), so leaving stdio attached costs nothing.
	redirect := ""
	// cgroup placement happens first (netns-independent), then — for a packed VM —
	// enter its netns so firecracker opens the tap there.
	shell := fmt.Sprintf("mkdir -p %s && echo $$ > %s/cgroup.procs && exec %s%s %s %s",
		shellQuote(cgDir), shellQuote(cgDir), m.netnsExecPrefix(), shellQuote(bin), shellJoin(args), redirect)
	return "sh", []string{"-c", shell}
}

// netnsExecPrefix returns "ip netns exec <ns> " for a packed VM (so the wrapped
// firecracker runs in the VM's netns and can open its tap), or "" for the
// boot/base VM, which uses the pod netns. cgroup placement is done before this in
// the wrap, so entering the netns here doesn't disturb cgroup membership.
func (m *microvm) netnsExecPrefix() string {
	if m.netnsName == "" {
		return ""
	}
	return "ip netns exec " + shellQuote(m.netnsName) + " "
}

// cpuQuotaPeriodUs is the CFS period (µs) the CPU quota is expressed against;
// quota = vcpuCount * period caps the VMM at vcpuCount whole cores. Matches the
// container backend's runc resources (internal/runc.cpuQuotaPeriodUs).
const cpuQuotaPeriodUs = 100_000

// vmmMemoryHeadroomMiB is extra memory granted beyond the guest RAM so the
// firecracker VMM's own footprint — device models, page tables, and the
// snapshot/overlay mmaps — doesn't push the cgroup over memory.max and get the
// VMM OOM-killed. A container's cgroup caps the workload directly, but a VM's
// process RSS is guest RAM plus this emulation overhead.
const vmmMemoryHeadroomMiB = 256

// applyCgroupLimits caps the VMM's cgroup CPU and memory from the sandbox config
// (cgroup v2): CPU as a CFS quota of vcpuCount whole cores, memory as the
// configured size plus VMM headroom. Called before the VMM joins the cgroup
// (LaunchAgent/ResumeAgent). Best-effort: limits are a resource concern, not a
// correctness one, so a host without cgroup delegation (the child limit files
// absent) logs and runs the VM unconstrained rather than failing the launch.
func (m *microvm) applyCgroupLimits() {
	cgDir := filepath.Join("/sys/fs/cgroup", m.cgroupPath)
	if err := os.MkdirAll(cgDir, 0o755); err != nil {
		log.Printf("microvm: cgroup limits: create %s: %v", cgDir, err)
		return
	}
	// cpu.max/memory.max only exist in the child once the parent delegates the
	// controllers; enabling them is idempotent and best-effort.
	_ = os.WriteFile(filepath.Join(filepath.Dir(cgDir), "cgroup.subtree_control"), []byte("+cpu +memory"), 0)
	if m.vcpuCount > 0 {
		v := fmt.Sprintf("%d %d", m.vcpuCount*cpuQuotaPeriodUs, cpuQuotaPeriodUs)
		if err := os.WriteFile(filepath.Join(cgDir, "cpu.max"), []byte(v), 0); err != nil {
			log.Printf("microvm: cgroup limits: set cpu.max=%q: %v", v, err)
		}
	}
	if m.memSizeMib > 0 {
		limit := int64(m.memSizeMib+vmmMemoryHeadroomMiB) * 1024 * 1024
		if err := os.WriteFile(filepath.Join(cgDir, "memory.max"), []byte(strconv.FormatInt(limit, 10)), 0); err != nil {
			log.Printf("microvm: cgroup limits: set memory.max=%d: %v", limit, err)
		}
	}
}

// cgroupUnshareWrap is the pack-resume variant of cgroupWrap: after placing the
// process in the cgroup it enters a private mount namespace and bind-mounts each
// (src→dst) pair before exec'ing the VMM, so the VMM sees this VM's private
// overlay/vsock at the canonical paths the base snapshot recorded. make-rprivate
// keeps the binds from propagating back to the host (and to other VMs); the ns and
// its binds are torn down automatically when the VMM exits. The PID is preserved
// through the exec chain (sh→unshare→sh→firecracker), so the supervisor still
// tracks the firecracker process.
func (m *microvm) cgroupUnshareWrap(bin string, args []string, binds [][2]string) (string, []string) {
	cgDir := filepath.Join("/sys/fs/cgroup", m.cgroupPath)
	// firecracker MUST inherit the supervisor's stdio pipes — do not redirect them
	// to /dev/null. superviseStdio derives the agent-exit signal (agentDone) from
	// those pipes reaching EOF; a `>/dev/null` redirect closes them at exec time, so
	// agentDone fires immediately and the teardown goroutine SIGTERMs the VM ~200ms
	// after a resume (the silent "dies shortly after resume" bug — masked only by
	// FIRECRACKER_DEBUG_CONSOLE, which happens to also skip this redirect). In
	// non-console mode firecracker emits ~nothing here; the supervisor drains and
	// discards it (onStdio is nil), so leaving stdio attached costs nothing.
	redirect := ""
	var inner strings.Builder
	inner.WriteString("mount --make-rprivate / && ")
	for _, b := range binds {
		fmt.Fprintf(&inner, "mount --bind %s %s && ", shellQuote(b[0]), shellQuote(b[1]))
	}
	fmt.Fprintf(&inner, "exec %s %s %s", shellQuote(bin), shellJoin(args), redirect)
	// Enter the VM's netns (packed) before the private mount namespace, so the
	// resumed firecracker opens its tap in the netns and the binds land in a mount
	// ns nested inside it. cgroup placement stays first (netns-independent).
	shell := fmt.Sprintf("mkdir -p %s && echo $$ > %s/cgroup.procs && exec %sunshare --mount sh -c %s",
		shellQuote(cgDir), shellQuote(cgDir), m.netnsExecPrefix(), shellQuote(inner.String()))
	return "sh", []string{"-c", shell}
}

// reflinkCopy copies src to dst, preferring a copy-on-write reflink (fast, space-
// shared on XFS/btrfs) and falling back to a full copy where the filesystem can't
// reflink. Used to seed each packed VM's overlay from the shared base overlay so
// their writes diverge without duplicating the whole image up front.
func reflinkCopy(src, dst string) error {
	if out, err := exec.Command("cp", "--reflink=auto", "-f", src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("cp --reflink %s -> %s: %w (%s)", src, dst, err, bytes.TrimSpace(out))
	}
	return nil
}

// WaitReady blocks until the in-guest agent is ready (its readiness port
// accepts a connection) or ctx is cancelled.
func (m *microvm) WaitReady(ctx context.Context) error { return m.acceptReady(ctx) }

// acceptReady polls the guest's readiness port (GuestReadyPort), which the guest
// opens only once warm (sbxguest.serveReady, gated on prewarmReady). A successful
// TCP connect is the ready edge — readiness by polling, since the guest can no
// longer dial out over vsock. Shared by the cold-boot WaitReady and
// PrewarmSnapshot, which both wait for the same edge.
func (m *microvm) acceptReady(ctx context.Context) error {
	for {
		dialCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		conn, err := m.dialGuest(dialCtx, firecracker.GuestReadyPort)
		cancel()
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}


// PrewarmSnapshot boots a guest running the image entrypoint from the
// eagerly-prepared overlay (no workspaces yet — they aren't known), waits for it
// to signal ready (which sbxguest delays, on a prewarm boot, until the workload
// writes /run/hiver/prewarm-ready), pauses it, writes a full VM snapshot, and
// stops the transient VMM. The resumed guest inherits the warm, already-running
// workload. The overlay drive and snapshot/mem files are left in place for a
// later ResumeAgent. See the Isolation interface for how this fits the prewarm
// path.
func (m *microvm) PrewarmSnapshot(ctx context.Context, imgCfg *runc.ImageConfig) error {
	// Boot the workload with the image entrypoint and the prewarm flag: the guest
	// comes up, assembles its root, runs the entrypoint, and signals ready only
	// once the workload is warm. This also opens the readiness-beacon listener
	// (m.readyLn).
	bin, args, err := m.LaunchAgent(AgentConfig{ImageConfig: imgCfg, Prewarm: true})
	if err != nil {
		return fmt.Errorf("prepare prewarm boot: %w", err)
	}

	proc := exec.CommandContext(ctx, bin, args...)
	proc.Stdout, proc.Stderr = os.Stderr, os.Stderr
	if err := proc.Start(); err != nil {
		return fmt.Errorf("start prewarm vm: %w", err)
	}
	// Always tear the transient VM down and clear the API socket + vsock UDS so
	// ResumeAgent's fresh VMM can bind them. Idempotent (callable on the success
	// path and via defer).
	var stopOnce bool
	stop := func() {
		if stopOnce {
			return
		}
		stopOnce = true
		_ = proc.Process.Kill()
		_ = proc.Wait()
		_ = os.Remove(m.apiSock)
	}
	defer stop()

	client := firecracker.NewClient(m.apiSock)
	if err := client.WaitAPIReady(ctx); err != nil {
		return fmt.Errorf("prewarm vm api: %w", err)
	}
	if err := m.acceptReady(ctx); err != nil {
		return fmt.Errorf("prewarm vm ready: %w", err)
	}
	// Pause quiesces guest memory so the snapshot is consistent; CreateSnapshot
	// rejects a running VM.
	if err := client.Pause(ctx); err != nil {
		return fmt.Errorf("pause prewarm vm: %w", err)
	}
	if err := client.CreateSnapshot(ctx, m.snapFile, m.memFile); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	stop()

	m.mu.Lock()
	m.haveSnapshot = true
	m.mu.Unlock()
	return nil
}

// HasPrewarmSnapshot reports whether PrewarmSnapshot produced a snapshot.
func (m *microvm) HasPrewarmSnapshot() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.haveSnapshot
}

// ResumeAgent prepares a snapshot resume: it returns a cgroup-wrapped firecracker
// command started with only an API socket (no --config-file), into which
// ResumeReady loads the snapshot. The same overlay/tap/cgroup/vsock UDS from the
// prewarm boot are reused (the resume stays in-pod), so only the snapshot's
// machine + memory state needs restoring.
func (m *microvm) ResumeAgent() (string, []string, error) {
	// Firecracker opens the log file for appending rather than creating it.
	if err := os.WriteFile(m.logFile, nil, 0o644); err != nil {
		return "", nil, fmt.Errorf("create log file: %w", err)
	}
	m.applyCgroupLimits()
	bin, args := firecracker.CommandNoConfig(m.fcBin, m.apiSock)
	if m.baseDir == "" {
		// In-place prewarm resume: this VM reuses its own snapshot/overlay/tap/vsock
		// from PrewarmSnapshot, so no per-VM remapping is needed.
		wbin, wargs := m.cgroupWrap(bin, args)
		return wbin, wargs, nil
	}
	// Pack base resume: every VM loads the same base snapshot, which recorded the
	// base builder's canonical overlay path. Give this VM a private copy-on-write
	// overlay seeded from the base and bind it over that canonical path inside a
	// per-VM mount namespace so the VMM opens per-VM files. The tap is remapped on
	// load (ResumeReady); guest memory is the shared base mem.bin, kept COW-private
	// per VM by the File backend. (Host<->guest channels are TCP over the netns, so
	// there is no vsock dir to bind.)
	if err := os.MkdirAll(m.jailDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create jail dir: %w", err)
	}
	if err := reflinkCopy(filepath.Join(m.baseDir, baseOverlayName), m.overlayImg); err != nil {
		return "", nil, fmt.Errorf("seed overlay from base: %w", err)
	}
	binds := [][2]string{
		{m.overlayImg, filepath.Join(m.baseDir, baseOverlayName)},
	}
	wbin, wargs := m.cgroupUnshareWrap(bin, args, binds)
	return wbin, wargs, nil
}

// ResumeReady loads the prewarm snapshot into the VMM started by ResumeAgent and
// resumes the guest (resume_vm=true). The resumed guest is already past its
// readiness beacon, so a successful load is the ready edge — there is no beacon
// to wait on as on the cold path.
func (m *microvm) ResumeReady(ctx context.Context) error {
	client := firecracker.NewClient(m.apiSock)
	if err := client.WaitAPIReady(ctx); err != nil {
		return fmt.Errorf("resume vm api: %w", err)
	}
	// Configure the logger before loading the snapshot so the resume itself, and
	// any device-restore / guest fault on resume, lands in m.logFile. The cold
	// path gets this via its config-file; the resume path (CommandNoConfig) has no
	// logger otherwise, leaving firecracker.log empty when a resumed guest dies.
	// Level "Info" matches the cold path — high enough to capture a resume fault,
	// but without the per-request "Debug" fc_api spam, which under a concurrent
	// resume burst is real log I/O contending for the node's few cores.
	// Best-effort: a logging-setup failure shouldn't abort an otherwise-fine resume.
	if err := client.PutLogger(ctx, firecracker.Logger{LogPath: m.logFile, Level: "Info"}); err != nil {
		log.Printf("microvm: resume: configure logger: %v", err)
	}
	snap, mem := m.snapFile, m.memFile
	var overrides []firecracker.NetworkOverride
	if m.baseDir != "" {
		// Pack base resume: load the shared base snapshot + memory, and repoint the
		// snapshotted eth0 (recorded on the base builder's tap) at this VM's own tap
		// so its egress carries a distinct source IP. The overlay/vsock are already
		// remapped via the mount-namespace binds set up in ResumeAgent.
		snap = filepath.Join(m.baseDir, baseSnapshotName)
		mem = filepath.Join(m.baseDir, baseMemName)
		overrides = append(overrides, firecracker.NetworkOverride{IfaceID: "eth0", HostDevName: m.tapName})
	}
	if err := client.LoadSnapshot(ctx, snap, mem, true, overrides...); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}
	return nil
}

// ApplyResumeState delivers the post-resume setup the prewarm snapshot could not
// carry, in a single guest control RPC: the workload environment (env, so the
// guest's process PATH resolves the entrypoint and execs) and the config's
// workspaces (m.fuse, populated by ExportWorkspace during the post-resume
// reconcile). Both are unknown when the snapshot is taken; see ControlRequest.
func (m *microvm) ApplyResumeState(ctx context.Context, env []string) error {
	m.mu.Lock()
	fuse := append([]firecracker.GuestFuse(nil), m.fuse...)
	unmounts := append([]string(nil), m.pendingUnmount...)
	m.mu.Unlock()
	// Bind each workspace's host 9p listener now (the VM's netns exists and the
	// guest is resumed). The listener lives in the netns and the guest dials the
	// gateway over TCP, so — unlike the former 9p-over-vsock, which Firecracker's
	// /snapshot/load vsock re-init dropped — the mount survives resume and is
	// delivered live below via the control RPC. No console dependency.
	if len(fuse) > 0 {
		if err := m.bindWorkspaceListeners(); err != nil {
			return fmt.Errorf("bind workspace listeners: %w", err)
		}
	}
	// A pack base resume always needs the control RPC even with no env/workspaces:
	// the guest must be re-IP'd off the base snapshot's baked address to this VM's
	// own pod-local IP before it is marked ready.
	if len(env) == 0 && len(fuse) == 0 && len(unmounts) == 0 && m.baseDir == "" {
		return nil
	}
	// Self-heal: a just-resumed guest under load can reset the control connection
	// (the response never arrives) or apply the state only partially, leaving
	// /workspace unmounted. A single best-effort RPC would silently ship that
	// broken state. Instead re-drive the (idempotent) RPC with capped backoff
	// until the guest confirms OK; the caller marks the sandbox ready only once
	// this returns, so the pod converges to the correct state rather than serving
	// half-configured. Backoff is edge-triggered on failure, not a busy poll;
	// it ends when ctx is cancelled (sandbox teardown).
	backoff := 50 * time.Millisecond
	for attempt := 1; ; attempt++ {
		// Bound each attempt so a hung connection (accepted but never answered)
		// is abandoned and retried rather than blocking the self-heal forever.
		attemptCtx, cancel := context.WithTimeout(ctx, controlAttemptTimeout)
		err := m.applyResumeOnce(attemptCtx, env, fuse, unmounts)
		cancel()
		if err == nil {
			if attempt > 1 {
				log.Printf("microvm: resume state applied after %d attempts", attempt)
			}
			// The guest acked the unmounts; drop them so a later RPC doesn't resend.
			m.clearPendingUnmount(unmounts)
			return nil
		}
		if ctx.Err() != nil {
			// The VMM exit handler cancels this ctx (agentDone → cancel), so a resume
			// that dies mid-RPC lands here. Firecracker's own output is redirected to
			// /dev/null + firecracker.log (which teardown then deletes), so dump it
			// while it still exists — otherwise the crash is invisible.
			m.dumpResumeDiag(fmt.Sprintf("attempt %d failed, ctx done", attempt))
			return fmt.Errorf("apply resume state (after %d attempts): %w", attempt, err)
		}
		log.Printf("microvm: resume state attempt %d failed, retrying: %v", attempt, err)
		select {
		case <-time.After(backoff):
			if backoff < 2*time.Second {
				backoff *= 2
			}
		case <-ctx.Done():
			m.dumpResumeDiag("ctx done while backing off")
			return fmt.Errorf("apply resume state: %w", ctx.Err())
		}
	}
}

// dumpResumeDiag logs the tail of firecracker.log and a listing of the vsock dir
// (the per-port guest↔host sockets) when a resume fails. Firecracker's stdout/
// stderr go to /dev/null and its log to firecracker.log, which teardown removes
// moments later — so a VMM that dies on resume (e.g. faulting when the guest dials
// a host vsock port for a 9p workspace mount) leaves no trace otherwise. Best-
// effort: missing files just yield a short note.
func (m *microvm) dumpResumeDiag(reason string) {
	const tail = 4096
	logTail := "(unreadable)"
	if data, err := os.ReadFile(m.logFile); err == nil {
		if len(data) > tail {
			data = data[len(data)-tail:]
		}
		logTail = string(bytes.TrimSpace(data))
	} else {
		logTail = fmt.Sprintf("(read %s: %v)", m.logFile, err)
	}
	log.Printf("microvm: resume diag (%s):\nfirecracker.log tail:\n%s", reason, logTail)
}

// applyResumeOnce performs one control RPC: deliver env + workspaces + clock and
// read the guest's ack. A connection error, a lost response, or OK=false all
// return an error so ApplyResumeState retries. Idempotent guest-side (env/clock
// re-apply cleanly; mountWorkspaces skips already-mounted paths).
func (m *microvm) applyResumeOnce(ctx context.Context, env []string, fuse []firecracker.GuestFuse, unmounts []string) error {
	conn, err := m.dialGuest(ctx, firecracker.GuestControlPort)
	if err != nil {
		return fmt.Errorf("dial guest control: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// UnixNano corrects the guest's post-resume clock skew (frozen at snapshot
	// capture). Sent in the same RPC so it lands before the workload runs.
	req := firecracker.ControlRequest{Env: env, MountWorkspaces: fuse, UnmountWorkspaces: unmounts, UnixNano: time.Now().UnixNano()}
	if m.baseDir != "" {
		// Re-address eth0 to this VM's pod-local /30 (the base snapshot baked the
		// builder's address) so egress carries a distinct source for sbxproxy's
		// per-source ACL, and DNS follows the new gateway.
		req.GuestIP, req.GatewayIP = m.guestIP, m.gatewayIP
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send control request: %w", err)
	}
	var resp firecracker.ControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("read control response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("guest resume setup: %s", resp.Error)
	}
	return nil
}

// StopAgent is a no-op for the microvm backend: a resumed guest's VMM is the
// supervised child, stopped by cancelling its context on the resume teardown
// path (FlushAgent + the VMM stop), not through this method.
func (m *microvm) StopAgent(ctx context.Context) error { return nil }

// FlushAgent runs `sync` inside the guest so the agent's writes reach the
// overlay block device before the VM is stopped. reboot(POWER_OFF) and a host
// SIGTERM to firecracker do not flush the guest page cache, so without this the
// host would loop-mount a stale overlay image at capture time. The guest is
// still running here (the caller flushes before stopping it).
func (m *microvm) FlushAgent(ctx context.Context) error {
	cmd, cleanup, err := m.ExecCmd(ctx, ExecConfig{Command: "sync"})
	if err != nil {
		return err
	}
	defer cleanup()
	return cmd.Run()
}

// ExecCmd returns a command that bridges an exec session to the in-guest agent
// over TCP via the sbxvsock helper. The helper dials the guest exec port on the
// netns network (stamped with the egress SO_MARK), sends the command, and relays
// stdio + exit code, so the caller wires it exactly like the container backend's
// `runc exec`.
func (m *microvm) ExecCmd(ctx context.Context, cfg ExecConfig) (*exec.Cmd, func(), error) {
	host := m.guestIP
	if m.sourceIP != "" {
		host = m.sourceIP
	}
	args := []string{
		"-addr", net.JoinHostPort(host, strconv.Itoa(int(firecracker.GuestExecPort))),
		"-mark", strconv.Itoa(m.mark),
		"-command", cfg.Command,
	}
	if cfg.Cwd != nil && *cfg.Cwd != "" {
		args = append(args, "-cwd", *cfg.Cwd)
	}
	if cfg.TTY {
		args = append(args, "-tty")
	}
	if cfg.Env != nil {
		for k, v := range *cfg.Env {
			args = append(args, "-env", k+"="+v)
		}
	}
	cmd := exec.CommandContext(ctx, "sbxvsock", args...)
	// Killing sbxvsock (on ctx cancel) closes the vsock stream; the guest
	// agent reaps the workload child when its session ends, so no host-side
	// process-tree teardown is needed.
	return cmd, func() {}, nil
}

// buildParamsDrive serialises params to a small ext4 image with the file at
// firecracker.ParamsPath, which the guest agent mounts read-only.
func (m *microvm) buildParamsDrive(params firecracker.GuestParams) error {
	staging, err := os.MkdirTemp("", "sbx-params-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	dst := filepath.Join(staging, filepath.Base(firecracker.ParamsPath))
	b, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return err
	}
	return buildExt4FromDir(m.paramsImg, staging)
}

// buildExt4FromDir creates an ext4 image at img populated from srcDir, sized
// to the directory's contents plus headroom. Uses mke2fs -d, which copies a
// directory tree into a fresh filesystem without needing a loop mount.
func buildExt4FromDir(img, srcDir string) error {
	bytes, err := dirSizeBytes(srcDir)
	if err != nil {
		return err
	}
	sizeMiB := bytes/(1024*1024) + 64 // headroom for fs metadata + slack
	sizeMiB += sizeMiB / 2            // 1.5x for write room
	if err := truncateFile(img, sizeMiB); err != nil {
		return err
	}
	out, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-d", srcDir, img).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mke2fs -d %s: %w (%s)", srcDir, err, out)
	}
	return nil
}

// buildEmptyExt4 creates an empty ext4 image of sizeMiB megabytes.
func buildEmptyExt4(img string, sizeMiB int64) error {
	if err := truncateFile(img, sizeMiB); err != nil {
		return err
	}
	out, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", img).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mke2fs %s: %w (%s)", img, err, out)
	}
	return nil
}

// truncateFile creates (or resizes) img to sizeMiB megabytes as a sparse file.
func truncateFile(img string, sizeMiB int64) error {
	f, err := os.Create(img)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(sizeMiB * 1024 * 1024)
}

// dirSizeBytes sums the apparent size of regular files under dir.
func dirSizeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// envSlice merges image env (KEY=VAL slice) with sandboxd-injected extras
// (map), with extras taking precedence on key collisions.
func envSlice(imageEnv []string, extra map[string]string) []string {
	out := append([]string{}, imageEnv...)
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// dropIPv6Egress applies ip6tables rules that block the workload's IPv6 egress.
// The egress model (proxy redirect, DNS sink) is IPv4-only, so there is no v6
// path the proxy controls; without this, a routable v6 address (common on
// dual-stack k8s clusters) would let the workload egress straight around every
// control. It runs in sandboxd under CAP_NET_ADMIN — the same privilege the v4
// iptables rules use — so it needs no read-write /proc/sys and behaves the same
// under docker and k8s.
//
// If the kernel has no IPv6 (e.g. booted with ipv6.disable=1), ip6tables can't
// initialize its tables; that's the benign "nothing to block" case, so it is
// treated as a no-op rather than an error.
func dropIPv6Egress(ctx context.Context, rules [][]string) error {
	for _, args := range rules {
		out, err := exec.CommandContext(ctx, "ip6tables", args...).CombinedOutput()
		if err != nil {
			if bytes.Contains(out, []byte("Address family not supported")) {
				return nil // kernel has no IPv6; there is nothing to block
			}
			return fmt.Errorf("ip6tables %v: %w (%s)", args, err, bytes.TrimSpace(out))
		}
	}
	return nil
}

// enableIPForward ensures net.ipv4.ip_forward=1. It reads the current value
// first so it skips the write (and the permission requirement) when the host
// already has forwarding enabled — common on cloud nodes.
func enableIPForward() error {
	const proc = "/proc/sys/net/ipv4/ip_forward"
	v, err := os.ReadFile(proc)
	if err == nil && strings.TrimSpace(string(v)) == "1" {
		return nil
	}
	if err := os.WriteFile(proc, []byte("1"), 0); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	return nil
}

// enableRouteLocalnet ensures net.ipv4.conf.all.route_localnet=1 so the kernel
// delivers the guest's DNAT-to-127.0.0.1 egress instead of dropping it as a
// martian. Like enableIPForward it reads first and skips the write when already
// enabled: under docker the controller sets it via --sysctl (the container's
// /proc/sys is read-only), so this no-ops; under a privileged k8s pod /proc/sys
// is writable, so sandboxd sets it here and no pod sysctl / node allowlist is
// needed. Cloud nodes (e.g. GKE) often default it to 1 already.
func enableRouteLocalnet() error {
	const proc = "/proc/sys/net/ipv4/conf/all/route_localnet"
	v, err := os.ReadFile(proc)
	if err == nil && strings.TrimSpace(string(v)) == "1" {
		return nil
	}
	if err := os.WriteFile(proc, []byte("1"), 0); err != nil {
		return fmt.Errorf("enable route_localnet: %w", err)
	}
	return nil
}

// tapNameFor derives a stable, interface-name-safe tap device name from the
// pod hostname (Linux caps interface names at 15 bytes).
func tapNameFor(hostname string) string {
	name := "fctap-" + hostname
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

// shellQuote/shellJoin produce a minimally-safe single-quoted form for the
// `sh -c` wrapper that places firecracker in its cgroup. Inputs are
// host-controlled paths/flags, not user data, but quoting keeps spaces safe.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}
