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

	"github.com/blasten/hive/internal/firecracker"
	"github.com/blasten/hive/internal/vsockexec"
	"github.com/creack/pty"
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

	// Serve exec sessions to the host for the lifetime of the workload.
	go serveExec(firecracker.GuestExecPort)

	code := runWorkload(params)
	log.Printf("workload exited with code %d", code)

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
func assembleRoot() error {
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

	// Carry the pseudo-filesystems and metadata mount into the new root,
	// then pivot_root so the workload sees the merged view as /.
	for _, d := range []string{"/proc", "/sys", "/dev", metadataMnt} {
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
	_ = syscall.Unmount("/.oldroot", syscall.MNT_DETACH)
	_ = os.Remove("/.oldroot")
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
		return execTTY(conn, cmd)
	}
	return execPipes(conn, cmd)
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

func execTTY(conn io.ReadWriter, cmd *exec.Cmd) error {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()

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
