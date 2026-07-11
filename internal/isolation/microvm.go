package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/hiver-sh/hiver/internal/vsockexec"
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

	// Host-side artifact paths. apiSock/configFile/logFile live in the ephemeral
	// jailDir; rootfsImg is the shared read-only image. overlayImg/paramsImg/
	// snapFile/memFile live in stateDir when this VM is keyed (snapshot.vm.key),
	// else in the jail.
	jailDir    string
	apiSock    string
	configFile string
	logFile    string
	rootfsImg  string
	overlayImg string
	paramsImg  string
	snapFile   string
	memFile    string

	// stateDir, when non-empty (Config.VMStateDir), is this VM's persistent state
	// directory under the snapshot dir (/snapshots/vm-<key>). The writable overlay,
	// metadata drive, and firecracker snapshot.bin/mem.bin all live here, so a
	// snapshot captures in place and a resume reopens these exact paths directly —
	// no copy, no per-VM CoW. Empty keeps the overlay ephemeral in the jail.
	stateDir string

	// ephemeralStateDir marks stateDir as an auto-assigned (random-key) home for a
	// keyless VM (Config.VMStateEphemeral): it lets a keyless VM still be snapshotted,
	// but is removed in UnmountRoot since the client never asked to persist it. A
	// snapshot to a named key relocates the VM onto that (persistent) dir and clears
	// this flag (removing the old ephemeral dir), so a deliberately-keyed snapshot
	// survives shutdown. Guarded by mu (a snapshot may flip it concurrently).
	ephemeralStateDir bool

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

	// prealloc marks a packed VM claiming a preallocated network slot
	// (netns/veth/tap/iptables + DNS sink) provisioned ahead of time by the pod's
	// prealloc pool (Config.Prealloc) — only the network is preallocated, not the
	// overlay. RedirectEgress is then a no-op, and UnmountRoot leaves the netns
	// teardown to the pool, which resets the slot and returns it.
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
	// ttyEntrypoint, when set, is a `tty: true` config's entrypoint override
	// recorded by ApplyResumeState: it is NOT launched guest-side over the control
	// channel (which has no terminal), but on demand by EntrypointTTYBridge, which
	// runs it as a tty exec session so /v1/exec-stream can attach to it. ttyEnv is
	// the workload env (KEY=VALUE) it runs with.
	ttyEntrypoint []string
	ttyCwd        string
	ttyEnv        []string
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

	// haveSnapshot gates the resume path: set in newMicroVM when Config.VMStateDir
	// holds a complete VM snapshot (a keyed snapshot the client captured).
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
		hostname:          cfg.Hostname,
		cgroupPath:        sandboxCgroupPath(cgroupName),
		jailDir:           jail,
		apiSock:           filepath.Join(jail, "firecracker.sock"),
		configFile:        filepath.Join(jail, "config.json"),
		logFile:           filepath.Join(jail, "firecracker.log"),
		rootfsImg:         filepath.Join(jail, "rootfs.ext4"),
		stateDir:          cfg.VMStateDir,
		ephemeralStateDir: cfg.VMStateEphemeral && cfg.VMStateDir != "",
		guestIP:           gIP,
		gatewayIP:         gwIP,
		guestMAC:          mac,
		vcpuCount:         cfg.VcpuCount,
		memSizeMib:        cfg.MemoryMiB,
		fcBin:             envOr("FIRECRACKER_BIN", "firecracker"),
		kernel:            envOr("FIRECRACKER_KERNEL", "/var/lib/firecracker/vmlinux"),
		tapName:           tap,
		netnsName:         netnsName,
		sourceIP:          sourceIP,
		prealloc:          prealloc,
		localMounts:       cfg.LocalMounts,
	}
	// The overlay/metadata/snapshot live in the per-key state dir when this VM is
	// keyed (snapshot.vm.key) — the source of truth, captured and reopened in place.
	// Otherwise they are ephemeral in the jail and the VM can't be VM-snapshotted.
	drivesDir := jail
	if m.stateDir != "" {
		drivesDir = m.stateDir
	}
	m.overlayImg = filepath.Join(drivesDir, baseOverlayName)
	m.paramsImg = filepath.Join(drivesDir, baseMetadataName)
	m.snapFile = filepath.Join(drivesDir, baseSnapshotName)
	m.memFile = filepath.Join(drivesDir, baseMemName)
	// A keyed VM whose state dir already holds a snapshot resumes from it (in place)
	// instead of cold-booting; HasPrewarmSnapshot reports this to the caller.
	if m.stateDir != "" && baseSnapshotReady(m.stateDir) {
		m.haveSnapshot = true
	}
	return m
}

// Artifact names inside a VM's state dir. snapshot.bin/mem.bin are the firecracker
// state + memory (written by the snapshot action); overlay.ext4 is the writable
// filesystem; metadata.ext4 is the params drive. firecracker bakes these paths
// into the snapshot and reopens them on restore — and because they live in the
// stable state dir (not the ephemeral jail), resume reopens them in place.
const (
	baseSnapshotName = "snapshot.bin"
	baseMemName      = "mem.bin"
	baseOverlayName  = "overlay.ext4"
	baseMetadataName = "metadata.ext4"
)

// snapshotControlTimeout bounds the VMM pause/create-snapshot/resume calls during
// a live snapshot. Generous because CreateSnapshot writes the full guest memory
// file, but finite so a wedged VMM can't strand the cycle (and the guest) forever.
const snapshotControlTimeout = 5 * time.Minute

// VMSnapshotReady reports whether dir holds a complete VM-state snapshot (state
// + memory + the warm overlay), so a caller can decide to resume from it instead
// of cold-booting. A keyed VM snapshot (written by the snapshot action) uses the
// same triple layout as a pack base snapshot, so resume reuses the base path.
func VMSnapshotReady(dir string) bool { return baseSnapshotReady(dir) }

// baseSnapshotReady reports whether dir holds a complete, resumable VM snapshot:
// the firecracker state + memory plus the overlay and metadata drives the restore
// reopens. snapshot.bin is written only by a successful snapshot action, so its
// presence (with the rest) means the dir holds a snapshot to resume; a state dir
// that only cold-booted (no snapshot.bin yet) is not ready, so the caller
// cold-boots into it.
func baseSnapshotReady(dir string) bool {
	for _, f := range []string{baseSnapshotName, baseMemName, baseOverlayName, baseMetadataName} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Size() == 0 {
			return false
		}
	}
	return true
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
	// A resume reopens the overlay already in the state dir (it IS the captured
	// filesystem), so there's nothing to build — skip the ~2 GiB format.
	if m.haveSnapshot {
		return nil
	}
	// Cold boot: build a fresh empty overlay where the drives live (the per-key
	// state dir for a keyed VM, else the jail). A keyed cold boot writes its overlay
	// into the state dir so a later snapshot captures it in place.
	if m.stateDir != "" {
		if err := os.MkdirAll(m.stateDir, 0o755); err != nil {
			return fmt.Errorf("create vm state dir: %w", err)
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
	// that. The jailDir is still removed; a keyed VM's overlay/snapshot live in the
	// state dir (under -snapshot-dir), the source of truth, and are NOT removed here.
	if m.netnsName != "" {
		if !m.prealloc {
			m.teardownPackedNetMicrovm(context.Background())
		}
	} else {
		_ = exec.Command("ip", "link", "del", m.tapName).Run()
	}
	// An auto-assigned (ephemeral) state dir is this VM's private home for a keyless
	// boot — the client never asked to persist it, so it is torn down with the VM. A
	// client-keyed dir, or one this VM adopted via a named snapshot, is the source of
	// truth and is deliberately kept. Snapshot a keyless VM to promote its overlay
	// before shutdown.
	m.mu.Lock()
	ephemeralDir := ""
	if m.ephemeralStateDir && m.stateDir != "" {
		ephemeralDir = m.stateDir
	}
	m.mu.Unlock()
	if ephemeralDir != "" {
		_ = os.RemoveAll(ephemeralDir)
	}
	// The jail holds only ephemeral runtime state (sockets, logs).
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

// BindWorkspaces binds the host-side 9p listeners after the per-VM netns exists,
// so a cold-booting guest's boot-time workspace mounts (from the params drive)
// reach a live listener. For a boot microvm (no netns) the listeners were already
// served at ExportWorkspace, so this is a no-op; the resume path binds via
// ApplyResumeState instead. Idempotent.
func (m *microvm) BindWorkspaces(ctx context.Context) error {
	if m.netnsName == "" {
		return nil
	}
	return m.bindWorkspaceListeners()
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

// SnapshotLive captures snapshot parts from the running guest without stopping
// it. It pauses the guest once so every part is read from one consistent,
// quiescent instant, then resumes: a paused guest issues no block-device writes,
// so the overlay can be safely copied and the VM state safely snapshotted. The
// files tarball is captured from a copy of the overlay (never the guest's in-use
// image) so the host loop-mount can't race the guest's filesystem. The caller
// flushes the guest (FlushAgent) before calling so the overlay is current.
func (m *microvm) SnapshotLive(ctx context.Context, vmDir, filesDst string, include []string) (bool, error) {
	if vmDir == "" && filesDst == "" {
		return false, nil
	}
	// A VM snapshot is captured into vmDir, and firecracker bakes the overlay/metadata
	// host paths into the snapshot and reopens them verbatim on resume — so those
	// drives must physically live in vmDir at capture time. They already do when this
	// sandbox owns vmDir (created with snapshot.vm.key, cold or resumed); otherwise —
	// a keyless sandbox (overlay in the ephemeral jail) or a re-key (resumed from key
	// A, snapshotting to key B) — they are relocated into vmDir below, under pause.
	// No key is required at sandbox creation: the key names the destination here.
	relocate := vmDir != "" && vmDir != filepath.Dir(m.snapFile)
	// The pause→snapshot→resume cycle must complete even if the caller's request
	// context is canceled mid-capture (e.g. the client hit its timeout): a skipped
	// Resume leaves the guest paused, which looks like the VM stopped. Insulate the
	// VMM control calls from request cancellation, bounding them with their own
	// timeout so a wedged VMM can't hang the cycle forever.
	vmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotControlTimeout)
	defer cancel()
	client := firecracker.NewClient(m.apiSock)

	// The workspace 9p mounts are LEFT MOUNTED in the snapshot. sbxguest owns the
	// kernel v9fs transport (a socketpair), so on resume the mount survives and the
	// guest only re-establishes the host-facing connection (ReconnectWorkspaces) —
	// no unmount/remount, so the workload's cwd is never stranded. The caller flushes
	// the guest (FlushAgent) before this, and an interactive workload is idle at
	// capture, so the 9p stream is quiescent at the pause (no in-flight request
	// straddles the cut — see the nineproxy quiesce assumption).
	if err := client.Pause(vmCtx); err != nil {
		return false, fmt.Errorf("pause guest: %w", err)
	}
	// Always resume, even on a capture error: a paused guest otherwise looks stopped.
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := client.Resume(vmCtx); err != nil {
			log.Printf("microvm: snapshot: resume guest: %v", err)
		}
	}
	defer restore()

	// VM snapshot: capture into vmDir. CreateSnapshot writes snapshot.bin/mem.bin
	// alongside the overlay + metadata drives, so firecracker records — and on resume
	// reopens — every drive at its real path. When relocating, materialise the drives
	// in vmDir and repoint the live VM at them first (under pause: no guest writes, so
	// the copy is a consistent instant) so the recorded paths are the vmDir paths.
	vmCaptured := false
	if vmDir != "" {
		snapFile, memFile := m.snapFile, m.memFile
		if relocate {
			if err := os.MkdirAll(vmDir, 0o755); err != nil {
				return false, fmt.Errorf("create vm snapshot dir: %w", err)
			}
			newOverlay := filepath.Join(vmDir, baseOverlayName)
			newParams := filepath.Join(vmDir, baseMetadataName)
			if err := cowOrCopy(m.overlayImg, newOverlay); err != nil {
				return false, fmt.Errorf("relocate overlay: %w", err)
			}
			if err := cowOrCopy(m.paramsImg, newParams); err != nil {
				return false, fmt.Errorf("relocate metadata: %w", err)
			}
			// Repoint the live VM's drives at the relocated copies so CreateSnapshot
			// records the vmDir paths. The live VM then runs off vmDir, becoming the
			// single owner of this key — the same end state as a sandbox created with
			// snapshot.vm.key set (and the same single-owner rule: don't resume this
			// key into another sandbox while this one still runs).
			if err := client.PatchDrive(vmCtx, firecracker.OverlayDriveID, newOverlay); err != nil {
				return false, fmt.Errorf("repoint overlay drive: %w", err)
			}
			if err := client.PatchDrive(vmCtx, firecracker.MetadataDriveID, newParams); err != nil {
				return false, fmt.Errorf("repoint metadata drive: %w", err)
			}
			snapFile = filepath.Join(vmDir, baseSnapshotName)
			memFile = filepath.Join(vmDir, baseMemName)
		}
		if err := client.CreateSnapshot(vmCtx, snapFile, memFile); err != nil {
			return false, fmt.Errorf("create vm snapshot: %w", err)
		}
		if relocate {
			// Adopt vmDir as this VM's state dir so later in-place snapshots to the
			// same key, and teardown (which preserves the state dir, not the jail),
			// act on vmDir. haveSnapshot now holds — vmDir carries a complete snapshot.
			// vmDir is a client-named key, so it is persistent: clear the ephemeral
			// flag (this VM now survives shutdown for resume). If the dir we are
			// leaving was the auto-assigned ephemeral one, remove it now — the live VM
			// no longer uses it (its drives were repointed at vmDir).
			m.mu.Lock()
			oldEphemeralDir := ""
			if m.ephemeralStateDir && m.stateDir != "" && m.stateDir != vmDir {
				oldEphemeralDir = m.stateDir
			}
			m.stateDir = vmDir
			m.ephemeralStateDir = false
			m.overlayImg = filepath.Join(vmDir, baseOverlayName)
			m.paramsImg = filepath.Join(vmDir, baseMetadataName)
			m.snapFile = snapFile
			m.memFile = memFile
			m.haveSnapshot = true
			m.mu.Unlock()
			if oldEphemeralDir != "" {
				_ = os.RemoveAll(oldEphemeralDir)
			}
		}
		vmCaptured = true
	}

	// Files snapshot is a separate, portable tarball. It loop-mounts a copy of the
	// overlay (taken under pause for a consistent instant) so the host never mounts
	// the guest's live image — independent of the VM snapshot above.
	var tempOverlay string
	if filesDst != "" {
		f, err := os.CreateTemp("", "sbx-snap-overlay-*.ext4")
		if err != nil {
			return vmCaptured, fmt.Errorf("temp overlay: %w", err)
		}
		tempOverlay = f.Name()
		f.Close()
		if err := copyFile(m.overlayImg, tempOverlay); err != nil {
			os.Remove(tempOverlay)
			return vmCaptured, fmt.Errorf("copy overlay: %w", err)
		}
	}
	// Resume before the (slower) files loop-mount + tar.
	restore()
	if tempOverlay != "" {
		defer os.Remove(tempOverlay)
		if err := m.withOverlayMountAt(tempOverlay, false, func(upper string, mounts []snapshot.MountSource) error {
			return snapshot.Capture(filesDst, upper, mounts, include)
		}); err != nil {
			return vmCaptured, fmt.Errorf("capture files: %w", err)
		}
	}
	return vmCaptured, nil
}

// copyFile copies src to dst, creating dst (truncating if present). Used to
// freeze a paused guest's overlay into a per-key VM snapshot or a temp file.
// cowOrCopy materialises dst as an INDEPENDENT copy of src, preferring a
// copy-on-write reflink (instant, shares extents until written) and falling back
// to a full byte copy on filesystems without reflink (e.g. ext4) or across
// devices (the keyless case: overlay in the jail, snapshot dir under
// -snapshot-dir). Independence matters: when re-keying (snapshotting a resumed
// VM to a new key), the source's existing snapshot must stay a usable restore
// point, so a shared-inode hardlink would be wrong — the live VM writing the new
// copy would corrupt the old snapshot's overlay. Used under guest pause so the
// materialised image is a consistent instant.
func cowOrCopy(src, dst string) error {
	if err := reflinkFile(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// RestoreSnapshot extracts a captured snapshot into the freshly built overlay
// drive before boot, so the guest comes up on the prior writable state. Must
// run after MountRoot (which created the empty overlay) and before LaunchAgent.
func (m *microvm) RestoreSnapshot(src string, include []string) error {
	return m.withOverlayMount(false, func(upper string, mounts []snapshot.MountSource) error {
		return snapshot.Restore(src, upper, mounts, include)
	})
}

// withOverlayMount loop-mounts the overlay drive image at a temp dir and
// invokes fn with the guest's overlay upper dir and the local-FUSE mount
// sources resolved inside it (matching how the guest lays them out under the
// overlay root). The mount is always unmounted and the temp dir removed.
func (m *microvm) withOverlayMount(readonly bool, fn func(upper string, mounts []snapshot.MountSource) error) error {
	return m.withOverlayMountAt(m.overlayImg, readonly, fn)
}

// withOverlayMountAt is withOverlayMount against an explicit overlay image path,
// so the live snapshot action can capture from a paused-and-copied overlay
// rather than the guest's in-use one.
func (m *microvm) withOverlayMountAt(overlayImg string, readonly bool, fn func(upper string, mounts []snapshot.MountSource) error) error {
	mp, err := os.MkdirTemp("", "sbx-snap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mp)

	// The sandbox container's /dev has no loop nodes (it is a tmpfs, not
	// devtmpfs, so the kernel never populates them). Create loop-control and a
	// pool of loop devices so loopMountExt4 can allocate one; the controller
	// grants the matching device-cgroup rules for microvm sandboxes.
	if err := ensureLoopNodes(); err != nil {
		return fmt.Errorf("provision loop devices: %w", err)
	}

	// Loop-mount the overlay via direct loop+mount syscalls (no `mount`/`umount`
	// subprocess). Always read-write, even for capture: the guest powers off without
	// cleanly unmounting its ext4 overlay, so the journal is dirty and a read-only
	// mount is refused ("cannot mount ... read-only") because the journal cannot be
	// replayed. The image is quiescent here (VM stopped), so a read-write mount
	// safely recovers it; capture only reads from it.
	loop, err := loopMountExt4(overlayImg, mp)
	if err != nil {
		return fmt.Errorf("mount overlay image: %w", err)
	}
	defer loopUnmount(mp, loop)

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

// loopDevicePoolSize is how many /dev/loopN nodes ensureLoopNodes creates for the
// files-snapshot loop-mount (withOverlayMountAt). `mount -o loop` allocates the
// first free one via /dev/loop-control; loop nodes are just device nodes — cheap
// to pre-create — so size generously to cover concurrent files captures.
const loopDevicePoolSize = 256

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

// cgroupWrap places the firecracker process in the sandbox cgroup (and, for a
// packed VM, its netns) before exec'ing it, so its CPU/memory (the guest runs as VMM
// threads) are accounted under CgroupPath. It re-execs ourselves as the
// namespace-launch helper (nsExecArgs / MaybeRunNSExec) instead of shelling out, so a
// launch is a SINGLE fork: the helper joins the cgroup/netns and execs firecracker in
// place. The PID is preserved through that exec as the supervised agent process, and
// firecracker inherits the supervisor's stdio pipes — which must NOT be redirected:
// superviseStdio derives agentDone from them reaching EOF, so a `>/dev/null` would
// fire agentDone at exec time and the teardown goroutine would SIGTERM the VM ~200ms
// after resume. The helper exec leaves stdio attached, so this is handled for free.
func (m *microvm) cgroupWrap(bin string, args []string) (string, []string) {
	return m.nsExecArgs(bin, args, nil)
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

// nsExecArg is argv[1] for the namespace-launch helper re-exec (see MaybeRunNSExec).
const nsExecArg = "__nsexec"

// selfExe is the path the helper re-execs: our own running binary.
var selfExe = "/proc/self/exe"

// nsExecArgs builds the argv to re-exec ourselves as the namespace-launch helper
// (MaybeRunNSExec): join the cgroup, optionally enter the VM netns, optionally
// unshare a private mount ns and bind the per-VM overlay, then exec bin+args. This is
// a single fork — the helper execs firecracker in place — replacing the former
// sh→ip netns exec→unshare→sh chain (4 forks). bind pairs are passed as src=dst
// (neither the dm device path nor the canonical overlay path contains '='); the cmd
// follows a "--" separator so the helper's flag parser stops before firecracker's
// own args.
func (m *microvm) nsExecArgs(bin string, args []string, binds [][2]string) (string, []string) {
	cgDir := filepath.Join("/sys/fs/cgroup", m.cgroupPath)
	out := []string{nsExecArg, "-cgroup", cgDir}
	if m.netnsName != "" {
		out = append(out, "-netns", m.netnsName)
	}
	for _, b := range binds {
		out = append(out, "-bind", b[0]+"="+b[1])
	}
	out = append(out, "--", bin)
	return selfExe, append(out, args...)
}

// WaitReady blocks until the in-guest agent is ready (its readiness port
// accepts a connection) or ctx is cancelled.
func (m *microvm) WaitReady(ctx context.Context) error { return m.acceptReady(ctx) }

// StreamWorkloadLogs dials the guest's logs port and publishes the entrypoint
// workload's stdout/stderr (captured guest-side by sbxguest.streamWorkloadLogs) via cb,
// reconnecting with backoff until ctx is done. The microvm analogue of the
// container backend forwarding the agent child's stdout: the workload runs inside
// the VM, so without this its output — including a crashing Chrome's stderr — dies
// on the guest console and never reaches the sandbox's stdio events. The guest
// replays a backlog on connect, so output from before the host attached (early
// startup, an immediate crash) is still delivered. Blocks; run in a goroutine.
// dialChannel opens a connection on the single guest port and selects a channel
// by writing its leading GuestChannel byte, so every host->guest protocol (exec,
// control, files, logs) multiplexes over one listener. Readiness is the exception:
// it dials the port bare (no byte) — a successful connect is the ready edge.
func (m *microvm) dialChannel(ctx context.Context, ch firecracker.GuestChannel) (net.Conn, error) {
	conn, err := m.dialGuest(ctx, firecracker.GuestPort)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte{byte(ch)}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (m *microvm) StreamWorkloadLogs(ctx context.Context, cb func(stream, chunk string)) {
	for ctx.Err() == nil {
		conn, err := m.dialChannel(ctx, firecracker.ChannelLogs)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		m.pumpWorkloadLogs(ctx, conn, cb)
	}
}

func (m *microvm) pumpWorkloadLogs(ctx context.Context, conn net.Conn, cb func(stream, chunk string)) {
	defer conn.Close()
	// Unblock the blocking ReadFrame when ctx ends (teardown).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	for {
		t, payload, err := vsockexec.ReadFrame(conn)
		if err != nil {
			return
		}
		stream := "stdout"
		if t == vsockexec.FrameStderr {
			stream = "stderr"
		}
		cb(stream, string(payload))
	}
}

// acceptReady polls the guest's single port (GuestPort, served by sbxguest once
// its agent is up). A bare connect — no channel byte — is the readiness probe: a
// successful TCP connect means the listener is up, i.e. the agent is serving.
// Readiness by polling, since the guest can no longer dial out over vsock.
func (m *microvm) acceptReady(ctx context.Context) error {
	for {
		dialCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		conn, err := m.dialGuest(dialCtx, firecracker.GuestPort)
		cancel()
		if err == nil {
			conn.Close() // bare connect (no channel byte) is the readiness probe
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// HasPrewarmSnapshot reports whether this VM's state dir holds a snapshot
// (set in newMicroVM), so the resume path is taken instead of a cold boot.
func (m *microvm) HasPrewarmSnapshot() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.haveSnapshot
}

// ResumeAgent prepares a VM-snapshot resume: it returns a cgroup-wrapped
// firecracker command started with only an API socket (no --config-file), into
// which ResumeReady loads the snapshot from the state dir. The overlay/metadata
// drives still live in the state dir at the paths firecracker recorded, so the
// resume reopens them in place — no copy, no per-VM CoW, no bind. (Single owner
// per key: the overlay is written in place by the resumed VM.) The tap is remapped
// on load (ResumeReady).
func (m *microvm) ResumeAgent() (string, []string, error) {
	// Firecracker opens the log file for appending rather than creating it.
	if err := os.WriteFile(m.logFile, nil, 0o644); err != nil {
		return "", nil, fmt.Errorf("create log file: %w", err)
	}
	m.applyCgroupLimits()
	bin, args := firecracker.CommandNoConfig(m.fcBin, m.apiSock)
	if err := os.MkdirAll(m.jailDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create jail dir: %w", err)
	}
	wbin, wargs := m.cgroupWrap(bin, args)
	return wbin, wargs, nil
}

// ResumeReady loads the staged VM snapshot into the VMM started by ResumeAgent and
// resumes the guest (resume_vm=true). The resumed guest is already serving, so a
// successful load is the ready edge — there is no beacon to wait on as on the cold
// path.
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
	// Load the snapshot + memory from the state dir, and repoint the snapshotted
	// eth0 (recorded on the capturing VM's tap) at this VM's own tap so its egress
	// carries a distinct source IP. The overlay/metadata drives are reopened at
	// their recorded state-dir paths directly.
	override := firecracker.NetworkOverride{IfaceID: "eth0", HostDevName: m.tapName}
	if err := client.LoadSnapshot(ctx, m.snapFile, m.memFile, true, override); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}
	return nil
}

// ApplyResumeState delivers the post-resume setup the VM snapshot could not carry
// — all unknown at snapshot time (see ControlRequest). It splits the work: the
// ready-critical state (workload env so guest execs resolve their PATH; the clock,
// frozen at capture; the per-VM re-IP) goes in a synchronous control RPC that
// gates readiness, while the config's workspace mounts (m.fuse) are applied
// ASYNCHRONOUSLY so a slow 9p mount doesn't hold up "ready". Deferring the mount is
// safe on the resume path: the entrypoint was started at base-build time, before
// any per-sandbox workspace existed, so it cannot depend on the mount at its own
// startup; cold boot (which can) mounts synchronously in bootstrap instead.
func (m *microvm) ApplyResumeState(ctx context.Context, rs ResumeState) error {
	m.mu.Lock()
	fuse := append([]firecracker.GuestFuse(nil), m.fuse...)
	unmounts := append([]string(nil), m.pendingUnmount...)
	m.mu.Unlock()
	// A tty override is not launched guest-side (the control channel has no
	// terminal); record it for EntrypointTTYBridge, which runs it as a tty exec
	// session the host wraps in a pty. A non-tty override is launched guest-side
	// over the control RPC below.
	guestEntrypoint, guestCmd, guestCwd := rs.Entrypoint, rs.Cmd, rs.Cwd
	if rs.Tty && len(rs.Entrypoint) > 0 {
		m.PrepareEntrypointTTY(append(append([]string(nil), rs.Entrypoint...), rs.Cmd...), rs.Cwd, rs.Env)
		guestEntrypoint, guestCmd, guestCwd = nil, nil, ""
	}
	// Phase 1 (gates readiness): env + clock + the per-VM re-IP + any non-tty
	// entrypoint override + queued unmounts, synchronously, self-healed until the
	// guest acks. A resume always needs this even with no env (the clock fix + re-IP),
	// hence the haveSnapshot condition.
	if len(rs.Env) > 0 || len(unmounts) > 0 || len(guestEntrypoint) > 0 || m.haveSnapshot {
		req := firecracker.ControlRequest{
			Env:               rs.Env,
			UnmountWorkspaces: unmounts,
			Entrypoint:        guestEntrypoint,
			Cmd:               guestCmd,
			WorkingDir:        guestCwd,
		}
		if err := m.applyControlRetry(ctx, "resume state", req); err != nil {
			return err
		}
		// The guest acked the unmounts; drop them so a later RPC doesn't resend.
		m.clearPendingUnmount(unmounts)
	}
	if len(fuse) == 0 {
		return nil
	}
	// Bring up the host 9p listeners (serveFuse enters the VM netns), then attach the
	// guest to them. Two cases:
	//
	//   - Initial resume (MountWorkspacesSync): the guest kept its 9p mounts alive
	//     across the snapshot (sbxguest owns the kernel transport via a socketpair),
	//     so the mount — and the entrypoint's cwd on it — never broke. We only
	//     RE-ESTABLISH the host-facing connection: the guest dials the re-bound
	//     listener and replays the live 9p session (ReconnectWorkspaces). Synchronous
	//     so the workspace is serving again before readiness. If the guest has no live
	//     proxy for a path (e.g. it was never mounted), it falls back to a fresh mount.
	//   - Live config-apply: a workspace ADDED to a running sandbox has no guest mount
	//     yet, so it is a fresh MountWorkspaces, applied asynchronously (idempotent
	//     re-drive) to keep the apply off the ready path.
	bindAndAttach := func(reconnect bool) error {
		tb := time.Now()
		if err := m.bindWorkspaceListeners(); err != nil {
			return fmt.Errorf("bind workspace listeners: %w", err)
		}
		log.Printf("microvm: bind ip=%s bind=%dms reconnect=%v", m.sourceIP, time.Since(tb).Milliseconds(), reconnect)
		req := firecracker.ControlRequest{MountWorkspaces: fuse}
		if reconnect {
			req = firecracker.ControlRequest{ReconnectWorkspaces: fuse}
		}
		return m.applyControlRetry(ctx, "workspace attach", req)
	}
	if rs.MountWorkspacesSync {
		return bindAndAttach(true)
	}
	go func() {
		if err := bindAndAttach(false); err != nil {
			log.Printf("microvm: async workspace mount: %v", err)
		}
	}()
	return nil
}

// applyControlRetry drives the post-resume control RPC (applyResumeOnce) with
// capped, edge-triggered backoff until the guest acks OK or ctx is cancelled.
// Shared by ApplyResumeState's ready-gating phase and its async workspace-mount
// phase; label distinguishes them in logs. A just-resumed guest under load can
// reset the connection or apply state only partially, so re-driving the
// (idempotent) RPC converges the pod to the correct state rather than shipping a
// half-configured one.
func (m *microvm) applyControlRetry(ctx context.Context, label string, req firecracker.ControlRequest) error {
	backoff := 50 * time.Millisecond
	for attempt := 1; ; attempt++ {
		// Bound each attempt so a hung connection (accepted but never answered) is
		// abandoned and retried rather than blocking forever.
		attemptCtx, cancel := context.WithTimeout(ctx, controlAttemptTimeout)
		err := m.applyResumeOnce(attemptCtx, req)
		cancel()
		if err == nil {
			if attempt > 1 {
				log.Printf("microvm: %s applied after %d attempts", label, attempt)
			}
			return nil
		}
		if ctx.Err() != nil {
			// The VMM exit handler cancels this ctx (agentDone → cancel), so a resume
			// that dies mid-RPC lands here. Firecracker's output is redirected to
			// /dev/null + firecracker.log (which teardown then deletes), so dump it
			// while it still exists — otherwise the crash is invisible.
			m.dumpResumeDiag(fmt.Sprintf("%s attempt %d failed, ctx done", label, attempt))
			return fmt.Errorf("%s (after %d attempts): %w", label, attempt, err)
		}
		log.Printf("microvm: %s attempt %d failed, retrying: %v", label, attempt, err)
		select {
		case <-time.After(backoff):
			if backoff < 2*time.Second {
				backoff *= 2
			}
		case <-ctx.Done():
			m.dumpResumeDiag("ctx done while backing off")
			return fmt.Errorf("%s: %w", label, ctx.Err())
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

// applyResumeOnce performs one control RPC: deliver the request (env, workspaces,
// entrypoint override) plus the per-attempt clock + re-IP, and read the guest's
// ack. A connection error, a lost response, or OK=false all return an error so
// ApplyResumeState retries. Idempotent guest-side (env/clock re-apply cleanly;
// mountWorkspaces skips already-mounted paths; the override launches at most once).
func (m *microvm) applyResumeOnce(ctx context.Context, req firecracker.ControlRequest) error {
	tDial := time.Now()
	conn, err := m.dialChannel(ctx, firecracker.ChannelControl)
	if err != nil {
		return fmt.Errorf("dial guest control: %w", err)
	}
	defer conn.Close()
	dialMs := time.Since(tDial).Milliseconds()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// UnixNano corrects the guest's post-resume clock skew (frozen at snapshot
	// capture). Stamped per attempt so it lands before the workload runs.
	req.UnixNano = time.Now().UnixNano()
	if m.haveSnapshot {
		// Re-address eth0 to this VM's pod-local /30 (the snapshot baked the
		// capturing VM's address) so egress carries a distinct source for sbxproxy's
		// per-source ACL, and DNS follows the new gateway.
		req.GuestIP, req.GatewayIP = m.guestIP, m.gatewayIP
	}
	tRPC := time.Now()
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
	log.Printf("microvm: apply split ip=%s dial=%dms rpc=%dms", m.sourceIP, dialMs, time.Since(tRPC).Milliseconds())
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
		"-addr", net.JoinHostPort(host, strconv.Itoa(int(firecracker.GuestPort))),
		"-mark", strconv.Itoa(m.mark),
		"-command", cfg.Command,
	}
	if cfg.Cwd != nil && *cfg.Cwd != "" {
		args = append(args, "-cwd", *cfg.Cwd)
	}
	if cfg.TTY {
		args = append(args, "-tty")
	}
	if cfg.SessionID != "" {
		args = append(args, "-session", cfg.SessionID)
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

// entrypointSessionID is the fixed detachable-session id for the tty entrypoint,
// so a resume re-attaches the warm process rather than relaunching it.
const entrypointSessionID = "entrypoint"

// PrepareEntrypointTTY records a tty entrypoint override so EntrypointTTYBridge
// can run it as a guest tty exec session. Used on the cold-boot path (where the
// guest console runs the image default keepalive); ApplyResumeState records the
// same on the resume path.
func (m *microvm) PrepareEntrypointTTY(argv []string, cwd string, env []string) {
	m.mu.Lock()
	m.ttyEntrypoint = append([]string(nil), argv...)
	m.ttyCwd = cwd
	m.ttyEnv = append([]string(nil), env...)
	m.mu.Unlock()
}

// EntrypointTTYBridge runs the recorded `tty: true` entrypoint override (stashed
// by ApplyResumeState or PrepareEntrypointTTY) as a tty exec session in the guest,
// returning the host bridge command the caller wraps in a pty so /v1/exec-stream
// can attach to it.
// The override runs under the guest's exec handler (a guest pty over the exec
// channel) rather than guest-side on the control channel, which has no terminal.
// Returns a nil command when the config requested no tty override.
func (m *microvm) EntrypointTTYBridge(ctx context.Context) (*exec.Cmd, func(), error) {
	m.mu.Lock()
	argv := append([]string(nil), m.ttyEntrypoint...)
	cwd := m.ttyCwd
	env := envSliceToMap(m.ttyEnv)
	m.mu.Unlock()
	if len(argv) == 0 {
		return nil, func() {}, nil
	}
	// The guest exec handler runs the command via `sh -c`; exec into the override
	// argv so the workload — not a lingering shell — is the terminal's foreground
	// process, matching how the cold/container path runs the entrypoint directly.
	// shellJoin single-quotes each arg so the exact argv survives the `sh -c`.
	command := "exec " + shellJoin(argv)
	// entrypointSessionID names the detachable session so a snapshot resume
	// re-attaches the warm entrypoint process instead of relaunching it cold.
	return m.ExecCmd(ctx, ExecConfig{Command: command, Cwd: &cwd, Env: &env, TTY: true, SessionID: entrypointSessionID})
}

// envSliceToMap turns "KEY=VALUE" entries into the map ExecConfig.Env wants.
func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
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
