package isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/blasten/hive/internal/firecracker"
	"github.com/blasten/hive/internal/runc"
	"github.com/blasten/hive/internal/snapshot"
)

// Guest network: a /30 point-to-point link over the tap device. The host
// owns the gateway address and the guest owns the other usable address; the
// guest routes all egress at the gateway, where the host REDIRECTs it to
// sbxproxy.
const (
	guestIP    = "172.16.0.2"
	gatewayIP  = "172.16.0.1"
	guestMAC   = "06:00:ac:10:00:02"
	guestVcpu  = 2
	guestMemMi = 1024
)

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
	vsockUDS   string
	configFile string
	logFile    string
	rootfsImg  string
	overlayImg string
	paramsImg  string

	// Toolchain + guest assets, resolved from env with defaults.
	fcBin   string // firecracker binary
	kernel  string // guest kernel (vmlinux)
	tapName string

	// localMounts are the host-side local-backend dirs (for snapshot).
	localMounts []SnapshotMount

	// Accumulated state from the capability calls, consumed by LaunchAgent.
	mu        sync.Mutex
	fuse      []firecracker.GuestFuse
	proxyPort int
	mark      int
	caCertPEM []byte
}

func newMicroVM(cfg Config) *microvm {
	jail := filepath.Join(envOr("FIRECRACKER_RUN_DIR", "/run/firecracker"), cfg.Hostname)
	return &microvm{
		hostname:    cfg.Hostname,
		cgroupPath:  sandboxCgroupPath(cfg.Hostname),
		jailDir:     jail,
		apiSock:     filepath.Join(jail, "firecracker.sock"),
		vsockUDS:    filepath.Join(jail, "vsock.sock"),
		configFile:  filepath.Join(jail, "config.json"),
		logFile:     filepath.Join(jail, "firecracker.log"),
		rootfsImg:   filepath.Join(jail, "rootfs.ext4"),
		overlayImg:  filepath.Join(jail, "overlay.ext4"),
		paramsImg:   filepath.Join(jail, "metadata.ext4"),
		fcBin:       envOr("FIRECRACKER_BIN", "firecracker"),
		kernel:      envOr("FIRECRACKER_KERNEL", "/var/lib/firecracker/vmlinux"),
		tapName:     tapNameFor(cfg.Hostname),
		localMounts: cfg.LocalMounts,
	}
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
	if err := buildEmptyExt4(m.overlayImg, 2048); err != nil { // 2 GiB writable upper
		return fmt.Errorf("build overlay image: %w", err)
	}
	return nil
}

func (m *microvm) UnmountRoot() error {
	// Best-effort teardown of the per-sandbox artifacts and tap link.
	_ = exec.Command("ip", "link", "del", m.tapName).Run()
	return os.RemoveAll(m.jailDir)
}

// ExportWorkspace starts a 9p-over-vsock server rooted at the host sbxfuse
// mount and records the mount + assigned vsock port for the guest to mount.
// Every guest workspace op then lands on the host FUSE daemon, reusing its
// ACL enforcement, audit events, and remote-backend handling.
func (m *microvm) ExportWorkspace(ctx context.Context, mount string) error {
	// ExportWorkspace runs before MountRoot (which also creates jailDir), so
	// ensure the jail exists before binding the 9p vsock socket under it.
	if err := os.MkdirAll(m.jailDir, 0o755); err != nil {
		return fmt.Errorf("create jail dir: %w", err)
	}

	m.mu.Lock()
	port := firecracker.GuestFuseBasePort + uint32(len(m.fuse))
	m.fuse = append(m.fuse, firecracker.GuestFuse{Mount: mount, Port: port})
	m.mu.Unlock()

	ln, err := firecracker.HostVsockListener(m.vsockUDS, port)
	if err != nil {
		return err
	}
	go func() {
		if err := firecracker.Serve9P(ctx, mount, ln); err != nil && ctx.Err() == nil {
			// Logged via the server's own path; nothing actionable here.
			_ = err
		}
	}()
	return nil
}

// InstallCA stashes the sandbox CA so LaunchAgent embeds it in the params
// drive; the guest agent splices it into the workload trust store at boot.
func (m *microvm) InstallCA(certPEM []byte) error {
	m.mu.Lock()
	m.caCertPEM = append([]byte(nil), certPEM...)
	m.mu.Unlock()
	return nil
}

// RedirectEgress provisions the host tap device that carries guest egress
// and installs the nat rules that funnel guest TCP to sbxproxy. The guest
// reaches the host at gatewayIP; the host receives the forwarded packets in
// PREROUTING (not OUTPUT — the guest is a separate network stack) and DNATs
// them to the proxy on loopback.
//
// DNAT to 127.0.0.1, not REDIRECT: sbxproxy listens on 127.0.0.1:proxyPort,
// but REDIRECT on a *forwarded* packet rewrites the destination to the
// incoming interface's primary address (gatewayIP), where nothing is
// listening — so the redirect would silently black-hole guest egress.
// DNAT'ing straight to 127.0.0.1 lands it on the proxy; route_localnet (set
// below) lifts the kernel's martian-drop of loopback-destined packets
// arriving on the tap, and SO_ORIGINAL_DST still recovers the guest's real
// destination from conntrack.
func (m *microvm) RedirectEgress(ctx context.Context, proxyPort, mark int) error {
	m.mu.Lock()
	m.proxyPort = proxyPort
	m.mark = mark
	m.mu.Unlock()

	steps := [][]string{
		{"ip", "tuntap", "add", "dev", m.tapName, "mode", "tap"},
		{"ip", "addr", "add", gatewayIP + "/30", "dev", m.tapName},
		{"ip", "link", "set", "dev", m.tapName, "up"},
		// DNS-over-TCP to the gateway is served by the in-pod relay (startDNSForwarder
		// below), not the proxy: exempt it from the redirect that follows.
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", m.tapName, "-p", "tcp", "--dport", guestDNSPort, "-j", "RETURN"},
		// Guest TCP arriving on the tap is DNAT'd to the host proxy on loopback.
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", m.tapName, "-p", "tcp", "-j", "DNAT", "--to-destination", fmt.Sprintf("127.0.0.1:%d", proxyPort)},
		// Proxy-originated upstream traffic (stamped with SO_MARK) escapes
		// any redirect so it isn't looped back.
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-m", "mark", "--mark", fmt.Sprintf("0x%x", mark), "-j", "RETURN"},
	}
	if err := enableIPForward(); err != nil {
		return err
	}
	for _, s := range steps {
		if out, err := exec.CommandContext(ctx, s[0], s[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w (%s)", s, err, out)
		}
	}
	// The guest's resolv.conf points at the tap gateway; relay its DNS to the
	// pod's own resolver, which the guest's separate network stack can't reach.
	// UDP DNS lands here directly (it isn't matched by the TCP redirect above);
	// TCP DNS is exempted by the RETURN rule.
	if err := startDNSForwarder(ctx, gatewayIP, podUpstreamDNS()); err != nil {
		return err
	}
	// route_localnet (needed so the kernel delivers the DNAT-to-127.0.0.1
	// packets instead of dropping them as martians) is enabled at pod-create
	// time via a docker --sysctl on net.ipv4.conf.all: the pod's /proc/sys is
	// read-only, so it can't be set here, and the kernel ORs the "all" value
	// over this tap anyway. See the controller's DockerRuntime.Start.
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
	etcResolv := resolvConfForGuest(podResolv, gatewayIP)

	m.mu.Lock()
	params := firecracker.GuestParams{
		Entrypoint:     cfg.ImageConfig.Entrypoint,
		Cmd:            cfg.ImageConfig.Cmd,
		Env:            envSlice(cfg.ImageConfig.Env, cfg.Env),
		WorkingDir:     cfg.ImageConfig.WorkingDir,
		Fuse:           m.fuse,
		ProxyPort:      m.proxyPort,
		Mark:           m.mark,
		ProxyAddr:      fmt.Sprintf("%s:%d", gatewayIP, m.proxyPort),
		CACertPEM:      m.caCertPEM,
		EtcHosts:       etcHosts,
		EtcResolvConf:  etcResolv,
		NodeCACertPath: NodeCACertPath,
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
			BootArgs:        firecracker.DefaultBootArgs(guestIP, gatewayIP),
		},
		MachineConfig: firecracker.MachineConfig{
			VcpuCount:  guestVcpu,
			MemSizeMib: guestMemMi,
			Smt:        false,
		},
		Drives: []firecracker.Drive{
			{DriveID: firecracker.RootDriveID, PathOnHost: m.rootfsImg, IsRootDevice: true, IsReadOnly: true},
			{DriveID: firecracker.OverlayDriveID, PathOnHost: m.overlayImg, IsRootDevice: false, IsReadOnly: false},
			{DriveID: firecracker.MetadataDriveID, PathOnHost: m.paramsImg, IsRootDevice: false, IsReadOnly: true},
		},
		NetworkInterfaces: []firecracker.NetworkInterface{
			{IfaceID: "eth0", HostDevName: m.tapName, GuestMAC: guestMAC},
		},
		Vsock:  &firecracker.Vsock{GuestCID: int(firecracker.GuestCID), UDSPath: m.vsockUDS},
		Logger: &firecracker.Logger{LogPath: m.logFile, Level: "Info"},
	}
	if err := firecracker.WriteConfigFile(m.configFile, fcCfg); err != nil {
		return "", nil, fmt.Errorf("write firecracker config: %w", err)
	}

	bin, args := firecracker.Command(m.fcBin, m.apiSock, m.configFile)
	// Place the firecracker process in the sandbox cgroup before exec so its
	// CPU/memory (the guest runs as VMM threads) are accounted under
	// CgroupPath, then hand off via exec so the PID — already in the cgroup —
	// is preserved as the supervised agent process.
	cgDir := filepath.Join("/sys/fs/cgroup", m.cgroupPath)
	// firecracker's stdout/stderr only ever carry the VMM banner ("Running
	// Firecracker ...") and the few early guest-kernel boot lines the serial
	// console emits before the cmdline disables it — all real workload I/O
	// (exec, file ops, TTYs) runs over vsock. That boot noise would otherwise
	// surface on the published sandbox log stream, so drop it. Gated by the
	// same FIRECRACKER_DEBUG_CONSOLE toggle as the serial console (DefaultBootArgs)
	// so it stays visible when you need to watch a boot failure.
	redirect := ">/dev/null 2>&1"
	if os.Getenv("FIRECRACKER_DEBUG_CONSOLE") != "" {
		redirect = ""
	}
	shell := fmt.Sprintf("mkdir -p %s && echo $$ > %s/cgroup.procs && exec %s %s %s",
		shellQuote(cgDir), shellQuote(cgDir), shellQuote(bin), shellJoin(args), redirect)
	return "sh", []string{"-c", shell}, nil
}

// WaitReady blocks until the in-guest agent is accepting exec sessions on
// its vsock port (it listens only after the workload root is assembled).
func (m *microvm) WaitReady(ctx context.Context) error {
	return firecracker.WaitGuestPort(ctx, m.vsockUDS, firecracker.GuestExecPort)
}

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

// ExecCmd returns a command that bridges an exec session to the in-guest
// agent over vsock via the sbxvsock helper. The helper performs the
// firecracker CONNECT handshake, sends the command, and relays stdio +
// exit code, so the caller wires it exactly like the container backend's
// `runc exec`.
func (m *microvm) ExecCmd(ctx context.Context, cfg ExecConfig) (*exec.Cmd, func(), error) {
	args := []string{
		"-uds", m.vsockUDS,
		"-port", strconv.Itoa(int(firecracker.GuestExecPort)),
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

