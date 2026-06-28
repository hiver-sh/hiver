//go:build linux

// Command sbxguest is the in-guest agent for the microvm isolation backend.
// It boots as the guest init (init=/usr/bin/sbxguest on the kernel cmdline),
// assembles the agent root filesystem, mounts the FUSE workspaces, installs
// the in-guest egress policy, launches the workload, and serves exec
// sessions to the host over vsock.
//
// It is the guest half of the host↔guest contract defined in
// internal/firecracker (GuestParams, the vsock exec port) and
// internal/vsockexec (the framed exec protocol). It cannot be exercised
// without a real Firecracker boot (guest kernel + KVM); the logic is
// structured so each step is independently auditable.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/hiver-sh/hiver/internal/firecracker"
	"github.com/hiver-sh/hiver/internal/guestsess"
	"github.com/hiver-sh/hiver/internal/nineproxy"
	"github.com/hiver-sh/hiver/internal/vsockexec"
	"github.com/hiver-sh/hiver/internal/vsockfile"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Guest block-device assignment, in the order the host PUTs the drives:
// rootfs (root, ro) = vda, overlay (rw) = vdb, metadata (ro) = vdc.
const (
	overlayDev  = "/dev/vdb"
	metadataDev = "/dev/vdc"

	metadataMnt = "/run/sbxguest"
	overlayMnt  = "/mnt/overlay"
	mergedMnt   = "/mnt/merged"

	// fuseHostGateway is the netns gateway (the guest's eth0 gateway / tap
	// address, = bootGatewayIP on the host) where the host runs each VM's 9p
	// listener. 9p is guest-initiated, so the guest dials the gateway. This is
	// fixed across resume because the netns model keeps the guest's baked IP.
	fuseHostGateway = "172.16.0.1"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("sbxguest: ")

	params, err := bootstrap()
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	// Apply the workload environment to this process so that LookPath and
	// os.Environ() (used in exec sessions) resolve binaries correctly, with a
	// guaranteed default PATH so bare commands resolve even when none is set.
	applyWorkloadEnv(params.Env)

	// Record the boot-time workspace mounts so the file API routes their paths to
	// the live 9p mounts and every other path to the overlay upper layer.
	// Resume-time mounts (added by the control RPC) register via mountWorkspaces.
	for _, fu := range params.Fuse {
		addFileWorkspaceMount(fu.Mount)
	}

	// One listener for every host->guest channel — exec, control RPC, file ops, and
	// the workload log stream — multiplexed by a leading GuestChannel byte (see
	// serveGuest). A bare connect (no byte) is the host's readiness probe: the
	// listener is only up once the agent is serving, so a successful connect is the
	// ready edge. The guest sees the assembled root (overlay + 9p workspaces), so
	// the host proxies every /v1/file* request here instead of reaching into
	// host-side backend dirs.
	go serveGuest(firecracker.GuestPort)

	code := runWorkload(params)
	log.Printf("workload exited with code %d", code)

	// Flush the overlay drive before power-off. reboot(POWER_OFF) does not sync
	// filesystems, so without this the agent's most recent writes (still in the
	// guest page cache) never reach the virtio block device backing the overlay
	// image — the host would then snapshot stale/zero-length files. sync()
	// blocks until writeback completes, so the data is on the image by the time
	// the host loop-mounts it for capture.
	unix.Sync()

	// As guest init, powering off makes the firecracker process exit, which
	// the host supervises as the agent's lifecycle end.
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
}

// bootstrap mounts the metadata drive, reads GuestParams, assembles the
// overlay root, sets up networking, mounts the workspaces over 9p, and pivots
// into the new root.
func bootstrap() (firecracker.GuestParams, error) {
	mountPseudoFS()
	tuneVM()

	if err := os.MkdirAll(metadataMnt, 0o755); err != nil {
		return firecracker.GuestParams{}, err
	}
	if err := syscall.Mount(metadataDev, metadataMnt, "ext4", syscall.MS_RDONLY, ""); err != nil {
		return firecracker.GuestParams{}, fmt.Errorf("mount metadata %s: %w", metadataDev, err)
	}
	params, err := readParams(filepath.Join(metadataMnt, filepath.Base(firecracker.ParamsPath)))
	if err != nil {
		return firecracker.GuestParams{}, err
	}

	if err := assembleRoot(); err != nil {
		return params, fmt.Errorf("assemble root: %w", err)
	}
	// After pivot_root, "/" is the merged workload root. Install the CA and
	// resolver config there before the workload starts, mirroring the
	// host-side work the container backend does into the merged rootfs.
	applyTrustAndResolver(params)
	if err := setupNetwork(params); err != nil {
		return params, fmt.Errorf("setup network: %w", err)
	}
	// Mount each workspace over 9p-over-vsock. sbxfuse runs on the host; the
	// guest just mounts the export, so every workspace op lands on the host
	// FUSE daemon (ACLs, audit, remote backends all stay host-side).
	mountWorkspaces(params.Fuse)
	return params, nil
}

// mountWorkspaces connects to each workspace's host 9p server over the netns
// network and mounts it at the workspace path with the trans=fd 9p transport. It
// is idempotent (a workspace already 9p-mounted is skipped) so the host can
// re-drive it to self-heal a partial/failed earlier apply, and returns the
// joined per-mount errors so the host knows whether the guest reached the
// desired state (and should retry) rather than assuming success.
func mountWorkspaces(mounts []firecracker.GuestFuse) error {
	var errs []error
	for _, f := range mounts {
		if f.Port == 0 {
			continue
		}
		// Register with the file API even on a resume (where this mount wasn't in the
		// boot params), so reads/writes/lists of /workspace route to the live 9p mount
		// rather than the overlay upper layer. Idempotent.
		addFileWorkspaceMount(f.Mount)
		if is9pMounted(f.Mount) {
			continue // already mounted by a prior apply — nothing to do
		}
		if err := os.MkdirAll(f.Mount, 0o755); err != nil {
			errs = append(errs, fmt.Errorf("9p %s: mkdir: %w", f.Mount, err))
			continue
		}
		if err := mountWorkspaceProxied(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// wsProxies holds the live 9p reconnect proxy for each mounted workspace, keyed
// by guest mount path. sbxguest — which survives a snapshot resume in guest RAM —
// owns these, so the kernel v9fs transport (a socketpair) stays up across a
// resume and only the host-facing 9p connection is re-established (reconnectWorkspaces).
var (
	wsMu      sync.Mutex
	wsProxies = map[string]*nineproxy.Proxy{}
)

// mountWorkspaceProxied mounts one workspace with kernel v9fs over a socketpair
// sbxguest owns, then proxies that socketpair to the host 9p listener via a
// nineproxy.Proxy. Because sbxguest holds the kernel-facing transport, a snapshot
// resume only drops the host-facing connection — the mount itself (and the
// workload's cwd on it) never breaks; reconnectWorkspaces re-establishes the host
// side and replays the 9p session.
func mountWorkspaceProxied(f firecracker.GuestFuse) error {
	// socketpair: one end becomes kernel v9fs's trans=fd transport, the other is
	// the proxy's. AF_UNIX/SOCK_STREAM is a reliable in-kernel byte stream that
	// survives the snapshot as plain fd state (no host backing to restore).
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("9p %s: socketpair: %w", f.Mount, err)
	}
	kernelFd, proxyFd := pair[0], pair[1]
	proxyConn, err := fdConn(proxyFd, "9p-proxy:"+f.Mount)
	if err != nil {
		unix.Close(kernelFd)
		return fmt.Errorf("9p %s: proxy fd: %w", f.Mount, err)
	}
	host, err := dialHostFuse(f.Port)
	if err != nil {
		proxyConn.Close()
		unix.Close(kernelFd)
		return fmt.Errorf("9p %s: dial host: %w", f.Mount, err)
	}
	// Start the proxy BEFORE the mount: v9fs performs its Tversion/Tattach
	// handshake synchronously inside the mount(2) syscall, so the proxy must
	// already be pumping the socketpair to the host or the mount deadlocks waiting
	// for a reply that never comes.
	px := nineproxy.NewProxy(proxyConn, host)
	wsMu.Lock()
	wsProxies[f.Mount] = px
	wsMu.Unlock()
	go func() {
		if err := px.Run(); err != nil {
			log.Printf("9p %s: proxy ended: %v", f.Mount, err)
		}
		wsMu.Lock()
		if wsProxies[f.Mount] == px {
			delete(wsProxies, f.Mount)
		}
		wsMu.Unlock()
	}()

	// Hand the kernel end to v9fs. The kernel fget's its own reference, but —
	// matching the prior trans=fd path's caution — we keep our copy open for the
	// mount's lifetime rather than risk closing the only reference.
	opts := firecracker.MountFuseOption(kernelFd)
	if err := syscall.Mount("sbxfuse", f.Mount, "9p", 0, opts); err != nil {
		px.Close() // tears down the proxy + host conn
		unix.Close(kernelFd)
		return fmt.Errorf("9p %s: mount: %w", f.Mount, err)
	}
	return nil
}

// reconnectWorkspaces re-establishes the host-facing 9p transport for each
// already-mounted workspace after a resume: it dials the re-bound host listener
// and replays the live 9p session onto it, leaving the kernel mount untouched. A
// workspace with no live proxy (e.g. not mounted before the snapshot) falls back
// to a fresh mount so it still appears.
func reconnectWorkspaces(mounts []firecracker.GuestFuse) error {
	var errs []error
	for _, f := range mounts {
		if f.Port == 0 {
			continue
		}
		addFileWorkspaceMount(f.Mount)
		wsMu.Lock()
		px := wsProxies[f.Mount]
		wsMu.Unlock()
		if px == nil {
			if err := mountWorkspaceProxied(f); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		host, err := dialHostFuse(f.Port)
		if err != nil {
			errs = append(errs, fmt.Errorf("9p %s: redial host: %w", f.Mount, err))
			continue
		}
		if err := px.Reconnect(host); err != nil {
			host.Close()
			errs = append(errs, fmt.Errorf("9p %s: reconnect: %w", f.Mount, err))
		}
	}
	return errors.Join(errs...)
}

// dialHostFuse opens a TCP connection to this VM's per-mount host 9p listener on
// the netns gateway.
func dialHostFuse(port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	sa := &unix.SockaddrInet4{Port: int(port)}
	copy(sa.Addr[:], net.ParseIP(fuseHostGateway).To4())
	if err := unix.Connect(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("connect gateway %s:%d: %w", fuseHostGateway, port, err)
	}
	return fdConn(fd, "host9p")
}

// fdConn wraps a connected socket fd as a net.Conn (with the runtime poller).
// net.FileConn dups the fd, so we close our os.File copy afterward.
func fdConn(fd int, name string) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("invalid fd %d", fd)
	}
	c, err := net.FileConn(f)
	f.Close()
	return c, err
}

// is9pMounted reports whether path is already a 9p mount, so a re-driven
// mountWorkspaces (self-heal) doesn't stack a second mount on it.
func is9pMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[1] == path && f[2] == "9p" {
			return true
		}
	}
	return false
}

// applyTrustAndResolver splices the sandbox CA into the workload trust store
// and writes the host-supplied /etc/hosts and /etc/resolv.conf, so a microvm
// agent trusts sbxproxy and resolves names exactly like a container agent.
func applyTrustAndResolver(p firecracker.GuestParams) {
	if len(p.CACertPEM) > 0 {
		bundle := "/etc/ssl/certs/ca-certificates.crt"
		if existing, err := os.ReadFile(bundle); err == nil {
			out := append(append(existing, '\n'), p.CACertPEM...)
			if err := os.WriteFile(bundle, out, 0o644); err != nil {
				log.Printf("install CA into %s: %v", bundle, err)
			}
		} else {
			log.Printf("guest rootfs has no %s — TLS interception will fail", bundle)
		}
		if p.NodeCACertPath != "" {
			_ = os.MkdirAll(filepath.Dir(p.NodeCACertPath), 0o755)
			if err := os.WriteFile(p.NodeCACertPath, p.CACertPEM, 0o644); err != nil {
				log.Printf("install Node CA %s: %v", p.NodeCACertPath, err)
			}
		}
	}
	// NSS clients (Chromium/Playwright) read neither the system bundle nor
	// NODE_EXTRA_CA_CERTS — they trust the per-user NSS db at $HOME/.pki/nssdb.
	// The host builds that db with certutil (the guest has no NSS tooling) and
	// ships the files in params.NSSDB; here we just drop them into the workload's
	// home, which lives in the writable overlay.
	installNSSDB(p.NSSDB)
	if len(p.EtcHosts) > 0 {
		if err := os.WriteFile("/etc/hosts", p.EtcHosts, 0o644); err != nil {
			log.Printf("write /etc/hosts: %v", err)
		}
	}
	if len(p.EtcResolvConf) > 0 {
		if err := os.WriteFile("/etc/resolv.conf", p.EtcResolvConf, 0o644); err != nil {
			log.Printf("write /etc/resolv.conf: %v", err)
		}
	}
}

// installNSSDB writes the host-built NSS database (files keyed by base name)
// into the workload's $HOME/.pki/nssdb so NSS clients (Chromium/Playwright)
// trust sbxproxy's minted leaf certs. The db is built host-side and shipped in
// params.NSSDB, so the guest needs no certutil of its own. HOME is uid 0's home
// from /etc/passwd, defaulting to /root. Best-effort: empty when the host had no
// certutil, or skipped for images that don't ship an NSS trust store.
func installNSSDB(files map[string][]byte) {
	if len(files) == 0 {
		return
	}
	nssdb := filepath.Join(workloadHome(), ".pki", "nssdb")
	if err := os.MkdirAll(nssdb, 0o700); err != nil {
		log.Printf("install NSS CA: mkdir %s: %v", nssdb, err)
		return
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(nssdb, name), b, 0o600); err != nil {
			log.Printf("install NSS CA: write %s: %v", name, err)
		}
	}
}

// workloadHome returns the home directory of uid 0 from /etc/passwd, defaulting
// to /root when it can't be determined.
func workloadHome() string {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "/root"
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		// name:passwd:uid:gid:gecos:home:shell
		if len(f) >= 6 && f[2] == "0" && f[5] != "" {
			return f[5]
		}
	}
	return "/root"
}

// mountPseudoFS mounts the kernel pseudo-filesystems the rest of bootstrap
// (and the workload) rely on. Failures are non-fatal — a minimal image may
// pre-mount some of these — so we log and continue.
func mountPseudoFS() {
	type m struct{ src, dst, fs string }
	for _, e := range []m{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
	} {
		_ = os.MkdirAll(e.dst, 0o755)
		if err := syscall.Mount(e.src, e.dst, e.fs, 0, ""); err != nil {
			log.Printf("mount %s: %v (continuing)", e.dst, err)
		}
	}

	// devpts provides the pseudo-terminal multiplexor: the TTY exec path
	// (creack/pty) opens /dev/ptmx to allocate a pty, which devtmpfs alone does
	// not supply. Mount it and point /dev/ptmx at the multiplexor so interactive
	// `tty: true` exec sessions work the same as on the container backend.
	_ = os.MkdirAll("/dev/pts", 0o755)
	if err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, "gid=5,mode=620,ptmxmode=666"); err != nil {
		log.Printf("mount /dev/pts: %v (continuing)", err)
	} else {
		_ = os.Remove("/dev/ptmx")
		if err := os.Symlink("pts/ptmx", "/dev/ptmx"); err != nil {
			log.Printf("link /dev/ptmx: %v (continuing)", err)
		}
	}
}

// tuneVM applies guest kernel sysctls the workload needs, before the prewarm
// snapshot is captured so every resumed warm pod inherits them. Currently it
// sets vm.overcommit_memory=1 (always overcommit): the guest is small and has
// no swap, so the default heuristic (mode 0) computes a low CommitLimit and
// __vm_enough_memory denies the large virtual reservations Chromium makes for a
// renderer — which aborts (trap int3) and crashes the page.
func tuneVM() {
	const proc = "/proc/sys/vm/overcommit_memory"
	if err := os.WriteFile(proc, []byte("1"), 0); err != nil {
		log.Printf("tuneVM: set overcommit_memory: %v (continuing)", err)
	}
}

func readParams(path string) (firecracker.GuestParams, error) {
	var p firecracker.GuestParams
	data, err := os.ReadFile(path)
	if err != nil {
		return p, fmt.Errorf("read params %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("parse params: %w", err)
	}
	return p, nil
}

// assembleRoot stacks the writable overlay drive on top of the read-only
// image root and pivot_roots into the merged view — the in-guest equivalent
// of the container backend's overlayfs mount.
//
// The overlay is an implementation detail and must not be reachable from the
// assembled root the workload runs in (just as the container backend keeps its
// upper dir host-side). Two things enforce that here: the overlay drive is
// detached from the mount namespace the instant the overlayfs is live (the
// overlayfs keeps its own kernel references, so upperdir/workdir survive with
// no path leading to them), and the old image root is made private and lazily
// detached after pivot_root so nothing under /.oldroot lingers.
func assembleRoot() error {
	// Make the whole tree private first: the guest's boot-time root may be a
	// shared mount, which would make the lazy unmounts below propagate or be
	// refused — leaving the overlay drive reachable. (Silent failure here was
	// why the upper layer leaked into the workload.)
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	if err := os.MkdirAll(overlayMnt, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(overlayDev, overlayMnt, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount overlay drive: %w", err)
	}
	upper := filepath.Join(overlayMnt, "upper")
	work := filepath.Join(overlayMnt, "work")
	for _, d := range []string{upper, work, mergedMnt} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// lowerdir="/" is the booted image root (the rootfs drive, read-only).
	opts := fmt.Sprintf("lowerdir=/,upperdir=%s,workdir=%s", upper, work)
	if err := syscall.Mount("overlay", mergedMnt, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlayfs: %w", err)
	}
	// Hold an fd on the upper dir before it leaves the namespace: the file API
	// serves the sandbox's own writes from here (overlayUpperRoot), so it never
	// exposes the read-only base image. The fd stays valid after the lazy
	// detach below — no path leads to the upper, but this handle does.
	upperRoot, err := os.OpenRoot(upper)
	if err != nil {
		return fmt.Errorf("open overlay upper: %w", err)
	}
	overlayUpperRoot = upperRoot
	// The overlayfs now pins the upper/work dirs internally, so the drive can
	// leave the namespace: detach it lazily. Writes still land on the (busy,
	// still-mounted) ext4 and flush on the shutdown sync; the workload simply
	// has no path to upperdir/workdir anymore.
	if err := syscall.Unmount(overlayMnt, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach overlay drive: %w", err)
	}

	// Carry only the pseudo-filesystems into the new root, then pivot_root so
	// the workload sees the merged view as /. The metadata drive (params.json:
	// the sandbox CA and full env) is deliberately NOT carried over — it was
	// consumed at boot, so leaving it out of the merged root keeps it, like the
	// overlay, an implementation detail the workload can't read.
	for _, d := range []string{"/proc", "/sys", "/dev"} {
		dst := filepath.Join(mergedMnt, d)
		_ = os.MkdirAll(dst, 0o755)
		_ = syscall.Mount(d, dst, "", syscall.MS_BIND|syscall.MS_REC, "")
	}
	oldRoot := filepath.Join(mergedMnt, ".oldroot")
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return err
	}
	if err := syscall.PivotRoot(mergedMnt, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return err
	}
	// Detach the old image root for good. Private first so the lazy unmount
	// drops the subtree instead of being silently refused.
	if err := syscall.Mount("", "/.oldroot", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make old root private: %w", err)
	}
	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	_ = os.Remove("/.oldroot")

	// Strip the raw overlay/metadata block-device nodes from the workload's
	// /dev as a final speed bump. Their filesystems stay mounted (the kernel
	// holds each bdev open regardless of the node), but with no /dev entry a
	// process can't re-open and re-mount them to reach the overlay upper or the
	// params drive. virtio majors are dynamically allocated, so recreating the
	// node by guesswork is impractical — best-effort, since this is hardening,
	// not correctness (a privileged process could still read the major from
	// /sys and mknod).
	for _, dev := range []string{overlayDev, metadataDev} {
		if err := os.Remove(dev); err != nil && !os.IsNotExist(err) {
			log.Printf("hide device node %s: %v", dev, err)
		}
	}

	// The overlay/merged/metadata mountpoints are baked into the read-only image
	// (the guest mounts onto them, and a read-only root can't mkdir them at
	// boot), but they're implementation details the workload shouldn't see. Now
	// that we're in the writable merged view, rmdir them: overlay records each
	// as a whiteout in the upper layer, hiding the empty lower dirs from the
	// workload (and the whiteout persists in snapshots).
	for _, d := range []string{overlayMnt, mergedMnt, metadataMnt} {
		if err := os.Remove(d); err != nil && !os.IsNotExist(err) {
			log.Printf("hide mountpoint %s: %v", d, err)
		}
	}
	return nil
}

// setupNetwork brings up the guest link and installs a minimal in-guest
// firewall rule. The kernel ip= cmdline arg already configured eth0's
// address, gateway, and default route; egress redirection to sbxproxy is
// enforced on the host tap (see the microvm backend's RedirectEgress). The
// in-guest mark-RETURN rule mirrors the host exemption so a future in-guest
// transparent step wouldn't loop proxy traffic.
func setupNetwork(p firecracker.GuestParams) error {
	_ = run("ip", "link", "set", "dev", "lo", "up")
	// Disable IPv6 on eth0 so its link-local DAD can't settle *after* a snapshot
	// resume. The base snapshot can capture eth0's fe80 address while DAD is still
	// running (flags TENTATIVE); when DAD completes on resume, the
	// tentative->permanent transition emits RTM_NEWADDR, which the workload's
	// network-change detector (Chrome's NetworkChangeNotifier, debounced a few
	// hundred ms) reads as a network change and aborts a navigation issued right
	// after resume with ERR_NETWORK_CHANGED. The guest is IPv4-only (the host drops
	// guest IPv6 egress), so removing eth0 IPv6 is pure hardening. This runs at cold
	// boot — before the workload starts and before the snapshot — so the snapshot,
	// and therefore every resumer, has no eth0 IPv6 left to settle. Best-effort.
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/eth0/disable_ipv6", []byte("1"), 0); err != nil {
		log.Printf("setupNetwork: disable eth0 ipv6: %v (continuing)", err)
	}
	if p.Mark != 0 {
		// Best-effort: absent iptables in a minimal guest, this is a no-op.
		_ = run("iptables", "-t", "nat", "-A", "OUTPUT", "-m", "mark",
			"--mark", fmt.Sprintf("0x%x", p.Mark), "-j", "RETURN")
	}
	return nil
}

// reconfigureEth0 re-addresses the guest's eth0 to guestIP on a /30 with the
// given gateway, replacing whatever the kernel ip= cmdline (or a base snapshot)
// configured, and repoints /etc/resolv.conf at the new gateway. Used by the
// pack-mode resume path, where every VM boots from the same base snapshot's baked
// address+gateway — including its DNS nameserver, since the host sinkholes guest
// DNS sent to the gateway.
//
// Each step is a no-op when eth0 is already in the desired state (the common case,
// since the guest keeps its baked IP and the host SNATs per-VM): this makes it
// idempotent for the host's self-heal retry AND avoids emitting spurious rtnetlink
// address/route change events that the workload's network-change detector would
// read as the link flapping (see the ERR_NETWORK_CHANGED note inline).
//
// Done via netlink rather than shelling out to `ip`: minimal guest images (e.g.
// the playwright sandbox) don't ship iproute2, so an `ip`-based re-IP failed the
// whole resume in a self-heal loop. netlink talks rtnetlink directly, with no
// in-guest binary dependency.
func reconfigureEth0(guestIP, gatewayIP string) error {
	link, err := netlink.LinkByName("eth0")
	if err != nil {
		return fmt.Errorf("eth0: %w", err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list eth0 addrs: %w", err)
	}
	// Idempotent re-address. Every pack resume re-applies the *same* baked address
	// (the guest keeps bootGuestIP; the host SNATs per-VM), so eth0 is already
	// correct on the common path. Only flush+re-add when it actually differs:
	// deleting and re-adding emits RTM_DELADDR/RTM_NEWADDR, which the workload's
	// network-change detector (e.g. Chrome's NetworkChangeNotifier) reads as the
	// interface dropping — surfacing as ERR_NETWORK_CHANGED on a navigation issued
	// right after resume, since that notification is processed asynchronously and
	// races the just-readied client's first request.
	want := guestIP + "/30"
	if !(len(addrs) == 1 && addrs[0].IPNet.String() == want) {
		for i := range addrs {
			if err := netlink.AddrDel(link, &addrs[i]); err != nil {
				return fmt.Errorf("flush eth0 addr %s: %w", addrs[i].IPNet, err)
			}
		}
		addr, err := netlink.ParseAddr(want)
		if err != nil {
			return fmt.Errorf("parse %s: %w", want, err)
		}
		if err := netlink.AddrReplace(link, addr); err != nil {
			return fmt.Errorf("add %s: %w", want, err)
		}
	}
	// Bring the link up only if it isn't already (a resumed guest's eth0 is); an
	// unconditional LinkSetUp re-emits RTM_NEWLINK.
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("link up eth0: %w", err)
		}
	}
	if gatewayIP != "" {
		gw := net.ParseIP(gatewayIP)
		if gw == nil {
			return fmt.Errorf("parse gateway %q", gatewayIP)
		}
		// Replace the default route only when it isn't already via gatewayIP — a
		// redundant RouteReplace also emits an rtnetlink event the workload may treat
		// as a network change.
		if !hasDefaultRouteVia(link, gw) {
			if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil {
				return fmt.Errorf("default route via %s: %w", gatewayIP, err)
			}
		}
		// The host DNATs guest DNS sent to the gateway to its sinkhole, so the
		// resolver must point at the gateway. Leave resolv.conf untouched when it
		// already does: cold boot wrote the full file (nameserver + the pod's
		// search/options lines, via resolvConfForGuest), so on resume the nameserver
		// is already the gateway. Rewriting it here — which also strips search/options
		// — is a DNS-config change the workload's resolver watcher (e.g. Chrome's
		// DnsConfigService) reads as a network change, surfacing as ERR_NETWORK_CHANGED
		// on a navigation issued right after resume. Only (re)write when it's wrong,
		// and only the minimal record we can reconstruct here.
		cur, _ := os.ReadFile("/etc/resolv.conf")
		if !resolvUsesNameserver(cur, gatewayIP) {
			if err := os.WriteFile("/etc/resolv.conf", []byte("nameserver "+gatewayIP+"\n"), 0o644); err != nil {
				return fmt.Errorf("rewrite resolv.conf: %w", err)
			}
		}
	}
	return nil
}

// resolvUsesNameserver reports whether resolv already lists gateway as a
// nameserver, so reconfigureEth0 can leave /etc/resolv.conf (and its search/options
// lines) untouched rather than rewriting it and triggering a DNS-config change.
func resolvUsesNameserver(resolv []byte, gateway string) bool {
	for _, line := range strings.Split(string(resolv), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" && f[1] == gateway {
			return true
		}
	}
	return false
}

// hasDefaultRouteVia reports whether link already has a default route (no Dst) via
// gw, so reconfigureEth0 can skip a redundant RouteReplace and its rtnetlink event.
func hasDefaultRouteVia(link netlink.Link, gw net.IP) bool {
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return false
	}
	for _, r := range routes {
		if r.Dst == nil && r.Gw != nil && r.Gw.Equal(gw) {
			return true
		}
	}
	return false
}

// runWorkload execs the agent image's entrypoint+cmd as a child, wiring its
// stdio to the guest console, and returns its exit code.
func runWorkload(p firecracker.GuestParams) int {
	argv := append(append([]string{}, p.Entrypoint...), p.Cmd...)
	return execWorkload(argv, p.WorkingDir)
}

// execWorkload execs argv as a child wired to the guest console (cwd defaulting
// to "/") and returns its exit code. Shared by the boot path (runWorkload) and
// the resume path (an entrypoint override delivered over the control channel).
func execWorkload(argv []string, cwd string) int {
	if len(argv) == 0 {
		log.Printf("no entrypoint/cmd; nothing to run")
		return 0
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	// Run with the corrected process environment, not the raw params: main called
	// applyWorkloadEnv(params.Env), which adds the HOME (and PATH) defaults the
	// image may omit. HOME is what points Chromium at the NSS trust db
	// ($HOME/.pki/nssdb) so it accepts sbxproxy's TLS interception — without it,
	// TLS interception is rejected. Exec sessions already run with os.Environ();
	// the entrypoint must match (the former prewarm hook inherited it by running
	// as sbxguest's child). The resume path re-applies the config's env over
	// os.Environ before reaching here, so an override entrypoint sees it too.
	cmd.Env = os.Environ()
	cmd.Dir = orDefault(cwd, "/")
	cmd.Stdin = os.Stdin
	// Tee stdout/stderr: keep the console copy (serial/kernel debugging) AND feed
	// the log hub so the host can stream them as stdio events (ChannelLogs).
	cmd.Stdout = io.MultiWriter(os.Stdout, logWriter{vsockexec.FrameStdout})
	cmd.Stderr = io.MultiWriter(os.Stderr, logWriter{vsockexec.FrameStderr})
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if asExit(err, &ee) {
			return ee.ExitCode()
		}
		log.Printf("workload: %v", err)
		return 1
	}
	return 0
}

// resumeWorkloadOnce guards the entrypoint-override launch: the control RPC
// self-heals (the host re-drives it until the guest acks), so without this a
// retry would spawn a second copy of the override workload.
var resumeWorkloadOnce sync.Once

// sessions holds the guest's detachable terminal sessions (the entrypoint tty).
// It lives for the agent's lifetime — and the agent survives a snapshot resume in
// guest RAM — so a session's process + pty persist across the host's exec
// connection dropping; a reconnect re-attaches the warm process.
var sessions = guestsess.New()

// startResumeWorkload launches the config's entrypoint override (delivered over
// the control channel on resume) exactly once, on console stdio. The prewarm
// snapshot booted the image's default entrypoint — a config-independent keepalive
// (e.g. agent-base's `tail -f /dev/null`) or resident service — which keeps the VM
// alive as runWorkload; the override is the workload the config actually asked
// for, so its exit ends the sandbox: sync the overlay and power off, exactly as a
// boot-path entrypoint exit does in main. Runs in a goroutine so the control RPC
// returns promptly to ack the host. A `tty: true` override is NOT delivered here —
// the host launches it as a tty exec session (a guest pty over the exec channel)
// so /v1/exec-stream can attach to its terminal.
func startResumeWorkload(req firecracker.ControlRequest) {
	resumeWorkloadOnce.Do(func() {
		argv := append(append([]string{}, req.Entrypoint...), req.Cmd...)
		log.Printf("resume: launching entrypoint override %q", argv)
		go func() {
			code := execWorkload(argv, req.WorkingDir)
			log.Printf("resume workload exited with code %d", code)
			unix.Sync()
			_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		}()
	})
}

// guestListenTCP binds a guest-network TCP listener on eth0 (all interfaces) for
// one of the agent's host->guest channels. The host reaches it by dialing this
// VM's source IP with SO_MARK (the ingress DNAT path); see the port doc in
// internal/firecracker. Replaces the former AF_VSOCK listeners, which did not
// survive a snapshot resume.
func guestListenTCP(name string, port uint32) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		log.Printf("%s: listen :%d: %v", name, port, err)
		return nil, err
	}
	log.Printf("%s: listening on tcp :%d", name, port)
	return ln, nil
}

// serveGuest is the single host->guest listener. Every channel — exec, control,
// file ops, workload logs — is multiplexed over one port: the host writes a
// leading GuestChannel byte, and each connection is dispatched to the matching
// handler. A connection that closes before sending a byte is the host's readiness
// probe (the connect edge is the signal), handled by dispatchGuestConn returning
// on EOF.
func serveGuest(port uint32) {
	ln, err := guestListenTCP("guest", port)
	if err != nil {
		return
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("guest: accept: %v", err)
			continue
		}
		go dispatchGuestConn(conn)
	}
}

// dispatchGuestConn reads the leading GuestChannel byte and hands the rest of the
// connection to the matching handler. EOF before the byte arrives is the readiness
// probe (connect+close) — a no-op.
func dispatchGuestConn(conn net.Conn) {
	defer conn.Close()
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		return // readiness probe or a dropped connection
	}
	switch firecracker.GuestChannel(b[0]) {
	case firecracker.ChannelExec:
		if err := handleExec(conn); err != nil && err != io.EOF {
			log.Printf("exec: session: %v", err)
		}
	case firecracker.ChannelControl:
		if err := handleControl(conn); err != nil && err != io.EOF {
			log.Printf("control: session: %v", err)
		}
	case firecracker.ChannelFiles:
		if err := handleFile(conn); err != nil && err != io.EOF {
			log.Printf("files: session: %v", err)
		}
	case firecracker.ChannelLogs:
		streamWorkloadLogs(conn)
	default:
		log.Printf("guest: unknown channel byte %d", b[0])
	}
}

// handleControl runs one control RPC: decode the request, apply it, and reply.
// Workspace mounting is best-effort (per-mount failures are logged in
// mountWorkspaces), so a successful round-trip reports OK.
func handleControl(conn io.ReadWriter) error {
	var req firecracker.ControlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return err
	}
	// Resync the clock first: a snapshot resume restores the guest clock to
	// capture time, so a warm pod's clock lags real time by its dwell — which
	// makes freshly-minted TLS leaf certs look not-yet-valid to in-guest clients.
	if req.UnixNano > 0 {
		setSystemClock(req.UnixNano)
	}
	// Apply the workload env before anything else so the resumed guest's process
	// PATH resolves the entrypoint and every subsequent exec — a prewarm guest
	// booted with none.
	if len(req.Env) > 0 {
		applyWorkloadEnv(req.Env)
	}
	// Re-address eth0 for a pack-mode resume: the base snapshot booted with the
	// base builder's IP, so this guest must take its own pod-local /30 before any
	// egress, matching the host tap the load was overridden onto. Idempotent
	// (flush+add+route replace), so the self-heal retry re-applies cleanly.
	if req.GuestIP != "" {
		if err := reconfigureEth0(req.GuestIP, req.GatewayIP); err != nil {
			log.Printf("control: re-ip eth0: %v", err)
			return json.NewEncoder(conn).Encode(firecracker.ControlResponse{OK: false, Error: err.Error()})
		}
	}
	// Launch the config's entrypoint override (unknown when the prewarm snapshot
	// was taken). After the clock/env/re-IP above so the override resolves its
	// command, sees the workload env, and has working egress; before the workspace
	// mounts below, which the prewarm model already treats as post-entrypoint
	// (they arrive in a later async RPC anyway). Guarded so the self-healing RPC
	// launches it at most once.
	if len(req.Entrypoint) > 0 {
		startResumeWorkload(req)
	}
	if len(req.ReconnectWorkspaces) > 0 {
		log.Printf("control: reconnecting %d workspace(s) post-resume", len(req.ReconnectWorkspaces))
		if err := reconnectWorkspaces(req.ReconnectWorkspaces); err != nil {
			// Report failure so the host re-drives this RPC. Reconnect is
			// idempotent-ish: a proxy already reconnected is replaced by a fresh
			// host dial+replay, which the quiesced session tolerates.
			log.Printf("control: reconnect workspaces: %v", err)
			return json.NewEncoder(conn).Encode(firecracker.ControlResponse{OK: false, Error: err.Error()})
		}
	}
	if len(req.MountWorkspaces) > 0 {
		log.Printf("control: mounting %d workspace(s) post-resume", len(req.MountWorkspaces))
		if err := mountWorkspaces(req.MountWorkspaces); err != nil {
			// Report failure so the host re-drives this RPC until the workspaces
			// are actually mounted — the mount is idempotent, so a retry only
			// completes the missing ones. Without this the guest would claim OK
			// while /workspace was never mounted.
			log.Printf("control: mount workspaces: %v", err)
			return json.NewEncoder(conn).Encode(firecracker.ControlResponse{OK: false, Error: err.Error()})
		}
	}
	if len(req.UnmountWorkspaces) > 0 {
		log.Printf("control: unmounting %d workspace(s)", len(req.UnmountWorkspaces))
		if err := unmountWorkspaces(req.UnmountWorkspaces); err != nil {
			log.Printf("control: unmount workspaces: %v", err)
			return json.NewEncoder(conn).Encode(firecracker.ControlResponse{OK: false, Error: err.Error()})
		}
	}
	return json.NewEncoder(conn).Encode(firecracker.ControlResponse{OK: true})
}

// unmountWorkspaces umounts each path (a workspace removed from the config) and
// removes its now-empty mountpoint, so the path disappears from the guest — the
// host already tore down the 9p export. Idempotent: a path that is no longer a 9p
// mount is skipped (its dir still removed best-effort), so the host can re-drive
// on a partial failure. A busy mount falls back to a lazy detach.
func unmountWorkspaces(paths []string) error {
	var errs []error
	for _, p := range paths {
		if is9pMounted(p) {
			if err := syscall.Unmount(p, 0); err != nil {
				if err2 := syscall.Unmount(p, syscall.MNT_DETACH); err2 != nil {
					errs = append(errs, fmt.Errorf("umount %s: %w", p, err))
					continue
				}
			}
		}
		_ = os.Remove(p) // drop the now-empty mountpoint; best-effort
	}
	return errors.Join(errs...)
}

// handleExec runs one exec session: read the Start frame, run the command
// (optionally under a pty), relay stdio frames, and send the exit code.
func handleExec(conn io.ReadWriter) error {
	t, payload, err := vsockexec.ReadFrame(conn)
	if err != nil {
		return err
	}
	if t != vsockexec.FrameStart {
		return fmt.Errorf("expected start frame, got %d", t)
	}
	var start vsockexec.Start
	if err := json.Unmarshal(payload, &start); err != nil {
		return fmt.Errorf("decode start: %w", err)
	}

	// A named session is detachable: the registry keeps the process + pty alive
	// across this connection dropping (a snapshot resume cuts the exec TCP), so a
	// later Start with the same id re-attaches the warm process instead of
	// relaunching it. The entrypoint tty uses this; ad-hoc execs (empty id) stay
	// one-shot.
	if start.SessionID != "" && start.TTY {
		return sessions.EnsureAttach(start.SessionID, guestsess.Spec{
			Command: start.Command,
			Cwd:     orDefault(start.Cwd, "/"),
			Env:     start.Env,
			TTY:     true,
		}, conn, start.Rows, start.Cols)
	}

	cmd := exec.Command("sh", "-c", start.Command)
	cmd.Dir = orDefault(start.Cwd, "/")
	cmd.Env = append(os.Environ(), envEntries(start.Env)...)

	if start.TTY {
		return execTTY(conn, cmd, start)
	}
	return execPipes(conn, cmd)
}

// defaultPath is applied when the workload environment defines no PATH, so bare
// commands (e.g. "tail") resolve against the standard binary locations even when
// neither the image nor the config ever sets PATH. Matches Docker's container
// default.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// applyWorkloadEnv sets each "KEY=VALUE" entry into this process's environment so
// exec.LookPath and os.Environ() (read by exec sessions) see the workload's env,
// then guarantees a PATH. A prewarm guest boots with an empty environment, and an
// image/config may omit PATH, so without this the entrypoint and every exec would
// fail to resolve their command. Called at boot from params.Env and again at
// resume with the first config's env (delivered over the control channel), so a
// restored guest matches a normally-booted one. Idempotent.
func applyWorkloadEnv(env []string) {
	hasHome := false
	for _, kv := range env {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			if kv[:idx] == "HOME" {
				hasHome = true
			}
			_ = os.Setenv(kv[:idx], kv[idx+1:])
		}
	}
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPath)
	}
	// Default HOME to uid 0's home when the workload env didn't set one, so
	// $HOME-relative lookups land where the boot installed their files — notably
	// the NSS trust db at $HOME/.pki/nssdb that Chromium reads to trust sbxproxy's
	// TLS interception. The kernel hands PID 1 a placeholder HOME=/, so a plain
	// os.Getenv("HOME") check wouldn't catch this — only the absence from the
	// workload env distinguishes "image set HOME" from the kernel default, which
	// would otherwise miss /root/.pki/nssdb.
	if !hasHome {
		_ = os.Setenv("HOME", workloadHome())
	}
}

// setSystemClock sets the guest wall clock to unixNano (host time), correcting
// the skew a snapshot resume leaves behind. Best-effort: a failure is logged,
// not fatal — TLS may still see a stale clock but nothing else breaks.
func setSystemClock(unixNano int64) {
	tv := unix.NsecToTimeval(unixNano)
	if err := unix.Settimeofday(&tv); err != nil {
		log.Printf("control: set clock: %v (continuing)", err)
	}
}

func execPipes(conn io.ReadWriter, cmd *exec.Cmd) error {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wmu sync.Mutex // serialise frame writes from both stdout/stderr pumps
	var wg sync.WaitGroup
	pump := func(r io.Reader, ft vsockexec.FrameType) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				wmu.Lock()
				_ = vsockexec.WriteFrame(conn, ft, buf[:n])
				wmu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout, vsockexec.FrameStdout)
	go pump(stderr, vsockexec.FrameStderr)

	// Host stdin frames → child stdin until StdinClose / session end.
	go relayStdin(conn, stdin)

	wg.Wait()
	code := waitCode(cmd)
	wmu.Lock()
	defer wmu.Unlock()
	return vsockexec.WriteJSON(conn, vsockexec.FrameExit, vsockexec.Exit{Code: code})
}

func execTTY(conn io.ReadWriter, cmd *exec.Cmd, start vsockexec.Start) error {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()

	// Apply the host's initial window size so the program starts at the right
	// dimensions; subsequent changes arrive as FrameResize via relayStdin.
	if start.Rows > 0 && start.Cols > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Rows: start.Rows, Cols: start.Cols})
	}

	go relayStdin(conn, ptmx)

	buf := make([]byte, 32*1024)
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			if werr := vsockexec.WriteFrame(conn, vsockexec.FrameStdout, buf[:n]); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	code := waitCode(cmd)
	return vsockexec.WriteJSON(conn, vsockexec.FrameExit, vsockexec.Exit{Code: code})
}

// relayStdin reads frames from the host and feeds Stdin frames to w, applies
// Resize frames to a pty, and closes w on StdinClose.
func relayStdin(conn io.ReadWriter, w io.WriteCloser) {
	for {
		t, payload, err := vsockexec.ReadFrame(conn)
		if err != nil {
			_ = w.Close()
			return
		}
		switch t {
		case vsockexec.FrameStdin:
			if _, werr := w.Write(payload); werr != nil {
				return
			}
		case vsockexec.FrameResize:
			if f, ok := w.(*os.File); ok {
				var ws vsockexec.Winsize
				if json.Unmarshal(payload, &ws) == nil {
					_ = pty.Setsize(f, &pty.Winsize{Rows: ws.Rows, Cols: ws.Cols})
				}
			}
		case vsockexec.FrameStdinClose:
			_ = w.Close()
			return
		}
	}
}

func waitCode(cmd *exec.Cmd) int {
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if asExit(err, &ee) {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

// handleFile handles one file operation per connection (the ChannelFiles handler)
// for the host-side /v1/file* API. Because the guest sees the workload root at
// real agent paths, a single handler serves every path uniformly — workspace mounts
// and the overlay alike.
// handleFile runs one file operation: read the Request frame, dispatch to the
// matching filesystem op, and reply with a Result (plus a Data/End body for
// read/write). Operation failures are reported in-band via Result.Err.
func handleFile(conn io.ReadWriter) error {
	t, payload, err := vsockfile.ReadFrame(conn)
	if err != nil {
		return err
	}
	if t != vsockfile.FrameRequest {
		return fmt.Errorf("expected request frame, got %d", t)
	}
	var req vsockfile.Request
	if err := json.Unmarshal(payload, &req); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	switch req.Op {
	case vsockfile.OpList:
		return fileList(conn, req.Path)
	case vsockfile.OpStat:
		return fileStat(conn, req.Path)
	case vsockfile.OpRead:
		return fileRead(conn, req.Path)
	case vsockfile.OpWrite:
		return fileWrite(conn, req.Path, req.Name)
	case vsockfile.OpDelete:
		return fileDelete(conn, req.Path)
	default:
		return fileErr(conn, fmt.Errorf("unknown op %q", req.Op))
	}
}

func fileErr(conn io.Writer, err error) error {
	return vsockfile.WriteJSON(conn, vsockfile.FrameResult, vsockfile.Result{Err: err.Error()})
}

// File-API routing state, set during bootstrap. The file API must expose only
// the sandbox's own writes, not the read-only base image: workspace paths are
// served from the live merged path (the host-backed 9p mount), and every other
// path from overlayUpperRoot (the overlay upper layer). This mirrors the
// container backend, whose file API reads the workspace backend dirs and the
// overlay upper dir directly — never the merged image.
var (
	// fileMountsMu guards fileWorkspaceMounts: it's seeded at bootstrap from the
	// boot params, but also appended to at runtime when the post-resume control RPC
	// mounts a workspace the base snapshot never had (see addFileWorkspaceMount),
	// concurrently with the file API's reads.
	fileMountsMu        sync.Mutex
	fileWorkspaceMounts []string
	overlayUpperRoot    *os.Root
)

// addFileWorkspaceMount registers a workspace mount path with the file API so
// resolveFile serves it from the live merged 9p mount instead of the overlay
// upper layer. Idempotent — a resume's self-heal can re-drive the mount.
func addFileWorkspaceMount(mount string) {
	if mount == "" {
		return
	}
	fileMountsMu.Lock()
	defer fileMountsMu.Unlock()
	for _, m := range fileWorkspaceMounts {
		if m == mount {
			return
		}
	}
	fileWorkspaceMounts = append(fileWorkspaceMounts, mount)
}

// resolveFile routes an agent path. Paths inside a workspace mount return
// (abs, "", false) to be served from the live merged path; all others return
// ("", rel, true) to be served within the overlay upper layer.
func resolveFile(agentPath string) (abs, rel string, useUpper bool) {
	clean := filepath.Clean("/" + agentPath)
	fileMountsMu.Lock()
	for _, m := range fileWorkspaceMounts {
		if clean == m || strings.HasPrefix(clean, strings.TrimRight(m, "/")+"/") {
			fileMountsMu.Unlock()
			return clean, "", false
		}
	}
	fileMountsMu.Unlock()
	rel = strings.TrimPrefix(clean, "/")
	if rel == "" {
		rel = "."
	}
	return "", rel, true
}

func fileList(conn io.Writer, path string) error {
	abs, rel, useUpper := resolveFile(path)
	var es []os.DirEntry
	var err error
	if useUpper {
		if overlayUpperRoot == nil {
			return fileErr(conn, fmt.Errorf("upper layer unavailable"))
		}
		// Enumerate the live merged root, then keep only entries that also exist in
		// the overlay upper layer — i.e. the sandbox's own writes. We can't getdents
		// the upper directly: its drive is detached (only the overlayUpperRoot fd
		// pins it) and after a snapshot resume that fd's getdents returns empty,
		// while named lookups on it still work. So enumerate the merged view (which
		// is live and reflects post-resume writes) and filter via per-name Stat on
		// the upper. The merged view already hides whiteouts (deletions).
		merged := filepath.Join("/", path)
		if es, err = os.ReadDir(merged); err == nil {
			kept := es[:0]
			for _, e := range es {
				if _, serr := overlayUpperRoot.Stat(filepath.Join(rel, e.Name())); serr == nil {
					kept = append(kept, e)
				}
			}
			es = kept
		}
	} else {
		es, err = os.ReadDir(abs)
	}
	if err != nil {
		return fileErr(conn, err)
	}
	entries := make([]vsockfile.Entry, 0, len(es))
	for _, e := range es {
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Overlay records a deleted lower file/dir as a whiteout (a char device
		// 0:0) in the upper layer. Skip them: they're overlay bookkeeping, not
		// files the workload can see, and would otherwise re-expose the names of
		// things we deliberately removed (e.g. the mnt/overlay mountpoint).
		if isOverlayWhiteout(info) {
			continue
		}
		var size int64
		if !e.IsDir() {
			size = info.Size()
		}
		entries = append(entries, vsockfile.Entry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}
	return vsockfile.WriteJSON(conn, vsockfile.FrameResult, vsockfile.Result{Entries: entries})
}

// isOverlayWhiteout reports whether info is an overlayfs whiteout — a
// character device with rdev 0:0, which overlay uses in the upper layer to
// mark a lower entry as deleted.
func isOverlayWhiteout(info os.FileInfo) bool {
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Rdev == 0
}

func fileStat(conn io.Writer, path string) error {
	abs, rel, useUpper := resolveFile(path)
	var info os.FileInfo
	var err error
	if useUpper {
		if overlayUpperRoot == nil {
			return fileErr(conn, fmt.Errorf("upper layer unavailable"))
		}
		info, err = overlayUpperRoot.Stat(rel)
	} else {
		info, err = os.Stat(abs)
	}
	if err != nil {
		return fileErr(conn, err)
	}
	var size int64
	if !info.IsDir() {
		size = info.Size()
	}
	e := vsockfile.Entry{Name: filepath.Base(path), IsDir: info.IsDir(), Size: size}
	return vsockfile.WriteJSON(conn, vsockfile.FrameResult, vsockfile.Result{Entry: &e})
}

func fileRead(conn io.Writer, path string) error {
	abs, rel, useUpper := resolveFile(path)
	var info os.FileInfo
	var fh *os.File
	var err error
	if useUpper {
		if overlayUpperRoot == nil {
			return fileErr(conn, fmt.Errorf("upper layer unavailable"))
		}
		if info, err = overlayUpperRoot.Stat(rel); err == nil {
			if !info.Mode().IsRegular() {
				return fileErr(conn, fmt.Errorf("not a regular file"))
			}
			fh, err = overlayUpperRoot.Open(rel)
		}
	} else {
		if info, err = os.Stat(abs); err == nil {
			if !info.Mode().IsRegular() {
				return fileErr(conn, fmt.Errorf("not a regular file"))
			}
			fh, err = os.Open(abs)
		}
	}
	if err != nil {
		return fileErr(conn, err)
	}
	defer fh.Close()
	if err := vsockfile.WriteJSON(conn, vsockfile.FrameResult, vsockfile.Result{Size: info.Size()}); err != nil {
		return err
	}
	buf := make([]byte, vsockfile.ChunkSize)
	for {
		n, rerr := fh.Read(buf)
		if n > 0 {
			if err := vsockfile.WriteFrame(conn, vsockfile.FrameData, buf[:n]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return vsockfile.WriteFrame(conn, vsockfile.FrameEnd, nil)
}

// fileWrite drains the whole data stream before replying so the host↔guest
// stream stays framed-aligned even when the create fails up front.
func fileWrite(conn io.ReadWriter, dir, name string) error {
	target := filepath.Join(dir, name)
	var out *os.File
	openErr := os.MkdirAll(dir, 0o755)
	if openErr == nil {
		out, openErr = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	}

	var written int64
	var writeErr error
	for {
		t, payload, rerr := vsockfile.ReadFrame(conn)
		if rerr != nil {
			if out != nil {
				out.Close()
				_ = os.Remove(target)
			}
			return rerr
		}
		if t == vsockfile.FrameEnd {
			break
		}
		if t != vsockfile.FrameData {
			if out != nil {
				out.Close()
				_ = os.Remove(target)
			}
			return fmt.Errorf("expected data frame, got %d", t)
		}
		if out != nil && writeErr == nil {
			if _, werr := out.Write(payload); werr != nil {
				writeErr = werr
			} else {
				written += int64(len(payload))
			}
		}
	}

	switch {
	case openErr != nil:
		return fileErr(conn, openErr)
	case writeErr != nil:
		out.Close()
		_ = os.Remove(target)
		return fileErr(conn, writeErr)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(target)
		return fileErr(conn, err)
	}
	return vsockfile.WriteJSON(conn, vsockfile.FrameResult, vsockfile.Result{Size: written})
}

func fileDelete(conn io.Writer, path string) error {
	err := os.Remove(path)
	if err != nil {
		return fileErr(conn, err)
	}
	return vsockfile.WriteJSON(conn, vsockfile.FrameResult, vsockfile.Result{})
}

// --- small helpers ---

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func envEntries(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func asExit(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
