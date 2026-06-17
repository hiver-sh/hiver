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
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/hiver-sh/hiver/internal/firecracker"
	"github.com/hiver-sh/hiver/internal/vsockexec"
	"github.com/hiver-sh/hiver/internal/vsockfile"
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

	// Record the workspace mounts so the file API routes their paths to the
	// live 9p mounts and every other path to the overlay upper layer.
	for _, fu := range params.Fuse {
		if fu.Mount != "" {
			fileWorkspaceMounts = append(fileWorkspaceMounts, fu.Mount)
		}
	}

	// Serve exec sessions and file operations to the host for the lifetime of
	// the workload. The guest sees the assembled root (overlay + 9p
	// workspaces), so the host proxies every /v1/file* request here instead of
	// reaching into host-side backend dirs.
	go serveExec(firecracker.GuestExecPort)
	go serveFiles(vsockfile.GuestPort)
	go serveControl(firecracker.GuestControlPort)

	// Prewarm idle mode: no entrypoint means this guest was booted only to be
	// snapshotted. Park serving exec/files/control forever (do NOT power off) so
	// the host can snapshot a ready VM, resume it later, mount the first config's
	// workspaces over the control channel, and run the entrypoint as an exec
	// session. The host powers the guest off (by stopping firecracker) when that
	// session ends.
	if len(params.Entrypoint) == 0 && len(params.Cmd) == 0 {
		log.Printf("no entrypoint; parking idle for snapshot/resume")
		select {}
	}

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

// mountWorkspaces connects to each workspace's host 9p server over vsock and
// mounts it at the workspace path with the trans=fd 9p transport. Failures
// are logged, not fatal.
func mountWorkspaces(mounts []firecracker.GuestFuse) {
	for _, f := range mounts {
		if f.Port == 0 {
			continue
		}
		if err := os.MkdirAll(f.Mount, 0o755); err != nil {
			log.Printf("9p %s: mkdir: %v", f.Mount, err)
			continue
		}
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			log.Printf("9p %s: vsock socket: %v", f.Mount, err)
			continue
		}
		if err := unix.Connect(fd, &unix.SockaddrVM{CID: firecracker.HostCID, Port: f.Port}); err != nil {
			log.Printf("9p %s: connect host port %d: %v", f.Mount, f.Port, err)
			unix.Close(fd)
			continue
		}
		// The kernel 9p fd transport takes over the socket; keep it open for
		// the mount's lifetime (no Close — the mount owns it).
		opts := firecracker.MountFuseOption(fd)
		if err := syscall.Mount("sbxfuse", f.Mount, "9p", 0, opts); err != nil {
			log.Printf("9p %s: mount: %v", f.Mount, err)
			unix.Close(fd)
		}
	}
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
	if p.Mark != 0 {
		// Best-effort: absent iptables in a minimal guest, this is a no-op.
		_ = run("iptables", "-t", "nat", "-A", "OUTPUT", "-m", "mark",
			"--mark", fmt.Sprintf("0x%x", p.Mark), "-j", "RETURN")
	}
	return nil
}

// runWorkload execs the agent image's entrypoint+cmd as a child, wiring its
// stdio to the guest console, and returns its exit code.
func runWorkload(p firecracker.GuestParams) int {
	argv := append(append([]string{}, p.Entrypoint...), p.Cmd...)
	if len(argv) == 0 {
		log.Printf("no entrypoint/cmd in params; nothing to run")
		return 0
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = p.Env
	cmd.Dir = orDefault(p.WorkingDir, "/")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
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

// serveExec listens on the guest vsock port and handles one exec session per
// connection using the vsockexec framed protocol.
func serveExec(port uint32) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		log.Printf("exec: vsock socket: %v", err)
		return
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		log.Printf("exec: vsock bind: %v", err)
		_ = unix.Close(fd)
		return
	}
	if err := unix.Listen(fd, 16); err != nil {
		log.Printf("exec: vsock listen: %v", err)
		_ = unix.Close(fd)
		return
	}
	log.Printf("exec: listening on vsock port %d", port)
	// The exec listener is up, so the host can now reach the agent: dial the
	// host's readiness beacon to unblock its WaitReady (replaces host-side
	// connect polling). Best-effort — the host falls back to its own timeout.
	signalReady()
	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			log.Printf("exec: accept: %v", err)
			continue
		}
		conn := os.NewFile(uintptr(nfd), "vsock-exec")
		go func() {
			defer conn.Close()
			if err := handleExec(conn); err != nil && err != io.EOF {
				log.Printf("exec: session: %v", err)
			}
		}()
	}
}

// serveControl listens on the guest vsock control port and handles host-issued
// control RPCs — currently mounting workspaces into the running guest after a
// snapshot resume (their 9p-over-vsock connections cannot survive the snapshot,
// so they are added live here). One request/response per connection.
func serveControl(port uint32) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		log.Printf("control: vsock socket: %v", err)
		return
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		log.Printf("control: vsock bind: %v", err)
		_ = unix.Close(fd)
		return
	}
	if err := unix.Listen(fd, 16); err != nil {
		log.Printf("control: vsock listen: %v", err)
		_ = unix.Close(fd)
		return
	}
	log.Printf("control: listening on vsock port %d", port)
	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			log.Printf("control: accept: %v", err)
			continue
		}
		conn := os.NewFile(uintptr(nfd), "vsock-control")
		go func() {
			defer conn.Close()
			if err := handleControl(conn); err != nil && err != io.EOF {
				log.Printf("control: session: %v", err)
			}
		}()
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
	// Apply the workload env before anything else so the resumed guest's process
	// PATH resolves the entrypoint and every subsequent exec — a prewarm guest
	// booted with none.
	if len(req.Env) > 0 {
		applyWorkloadEnv(req.Env)
	}
	if len(req.MountWorkspaces) > 0 {
		log.Printf("control: mounting %d workspace(s) post-resume", len(req.MountWorkspaces))
		mountWorkspaces(req.MountWorkspaces)
	}
	return json.NewEncoder(conn).Encode(firecracker.ControlResponse{OK: true})
}

// signalReady dials the host's readiness beacon (firecracker.GuestReadyPort)
// to tell the host the agent is up. The host listens on that port before boot,
// so the connection is the "ready" edge; the byte stream itself is unused.
func signalReady() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		log.Printf("ready: vsock socket: %v", err)
		return
	}
	defer unix.Close(fd)
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: firecracker.HostCID, Port: firecracker.GuestReadyPort}); err != nil {
		log.Printf("ready: connect host port %d: %v", firecracker.GuestReadyPort, err)
	}
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
	for _, kv := range env {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			_ = os.Setenv(kv[:idx], kv[idx+1:])
		}
	}
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPath)
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

// serveFiles listens on the guest vsock file port and handles one file
// operation per connection for the host-side /v1/file* API. Because the guest
// sees the workload root at real agent paths, a single handler serves every
// path uniformly — workspace mounts and the overlay alike.
func serveFiles(port uint32) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		log.Printf("files: vsock socket: %v", err)
		return
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		log.Printf("files: vsock bind: %v", err)
		_ = unix.Close(fd)
		return
	}
	if err := unix.Listen(fd, 16); err != nil {
		log.Printf("files: vsock listen: %v", err)
		_ = unix.Close(fd)
		return
	}
	log.Printf("files: listening on vsock port %d", port)
	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			log.Printf("files: accept: %v", err)
			continue
		}
		conn := os.NewFile(uintptr(nfd), "vsock-file")
		go func() {
			defer conn.Close()
			if err := handleFile(conn); err != nil && err != io.EOF {
				log.Printf("files: session: %v", err)
			}
		}()
	}
}

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
	fileWorkspaceMounts []string
	overlayUpperRoot    *os.Root
)

// resolveFile routes an agent path. Paths inside a workspace mount return
// (abs, "", false) to be served from the live merged path; all others return
// ("", rel, true) to be served within the overlay upper layer.
func resolveFile(agentPath string) (abs, rel string, useUpper bool) {
	clean := filepath.Clean("/" + agentPath)
	for _, m := range fileWorkspaceMounts {
		if clean == m || strings.HasPrefix(clean, strings.TrimRight(m, "/")+"/") {
			return clean, "", false
		}
	}
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
		var d *os.File
		if d, err = overlayUpperRoot.Open(rel); err == nil {
			defer d.Close()
			es, err = d.ReadDir(-1)
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
