// Command sandboxd is the prototype runtime agent that wires together the
// MITM proxy, FUSE daemon, and agent workload as a single sandbox "pod".
//
// sandboxd is configured by a single JSON spec file (see internal/spec).
// The spec carries everything sandboxd needs: the agent binary + args, the
// workspace's host-side backend and FUSE mount point, the FUSE ACLs, the
// proxy's egress allowlist, and where to write audit logs.
//
// Prototype scope (T47, T50): launch the three processes in the right order
// (proxy + FUSE first, then agent — DESIGN.md §3.3), wire env vars, prefix-
// stream stdio. Out of scope: real namespace/cgroup isolation (T49), CSI
// integration (T82), preflight checks (T51), credential broker (T60).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sandbox-platform/agent-sandbox/internal/api"
	"github.com/sandbox-platform/agent-sandbox/internal/runc"
	"github.com/sandbox-platform/agent-sandbox/internal/sandboxd"
	"github.com/sandbox-platform/agent-sandbox/internal/spec"
)

const mountReadTimout = 35 * time.Second

func main() {
	var (
		specPath      = flag.String("spec", "", "path to the sandbox spec JSON (required)")
		proxyBin      = flag.String("proxy-bin", "sbxproxy", "path to sbxproxy binary")
		fuseBin       = flag.String("fuse-bin", "sbxfuse", "path to sbxfuse binary")
		apiServerPort = flag.String("api-server-port", "8080", "port of the API server")
	)
	flag.Parse()

	if *specPath == "" {
		flag.Usage()
		log.Fatal("--spec is required")
	}
	sp, err := spec.Load(*specPath)
	if err != nil {
		log.Fatalf("spec: %v", err)
	}

	s := api.NewServer(*apiServerPort)
	go s.ListenAndServe()

	if err := os.MkdirAll(sp.AuditDir, 0o755); err != nil {
		log.Fatalf("create audit dir %s: %v", sp.AuditDir, err)
	}
	log.Printf("sandboxd: audit dir = %s", sp.AuditDir)

	for i := range sp.FS {
		f := &sp.FS[i]
		if err := os.MkdirAll(f.Mount, 0o755); err != nil {
			log.Fatalf("create mount point %s: %v", f.Mount, err)
		}
		if err := os.MkdirAll(f.BackendPath(), 0o755); err != nil {
			log.Fatalf("create backend %s: %v", f.BackendPath(), err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	proxyPort, err := freePort()
	if err != nil {
		log.Fatalf("free port: %v", err)
	}
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", proxyPort)
	// soMark stamps proxy-originated upstream sockets; the iptables
	// REDIRECT rule we install below uses `-m mark` to skip them and
	// avoid an infinite loop. Any non-zero value works; we pick a
	// distinctive one for grep-ability.
	const soMark = 0x5b1
	proxyAuditPath := filepath.Join(sp.AuditDir, "proxy.log")

	// As the orchestrator, sandboxd is responsible for reporting what
	// the agent does — the agent itself produces no application-level
	// logs. We tail the proxy and FUSE audit streams (the canonical
	// per-operation record per DESIGN.md §9.1) and surface each event
	// as a single "agent op | …" line on sandboxd's stdout.
	go tailAudit(ctx, proxyAuditPath, formatProxyEvent)
	for i := range sp.FS {
		go tailAudit(ctx, fuseAuditPathFor(sp.AuditDir, &sp.FS[i]), formatFuseEvent)
	}

	var children sync.WaitGroup

	// 1. sbxproxy in transparent mode. iptables (set up below) will
	// REDIRECT all outbound TCP from agent processes here; sbxproxy
	// recovers the original destination via SO_ORIGINAL_DST and
	// dispatches by protocol sniff. The agent itself is unaware of
	// the proxy — no HTTP_PROXY env, no opt-in cooperation required.
	rulesTmp := filepath.Join(sp.AuditDir, "egress-rules.json")
	if err := writeJSON(rulesTmp, sp.Egress.Allow); err != nil {
		log.Fatalf("write rules: %v", err)
	}

	caCertPath := filepath.Join(sp.AuditDir, "sandbox-ca.crt")
	caKeyPath := filepath.Join(sp.AuditDir, "sandbox-ca.key")

	caCert := sandboxd.GenerateCaCert(caCertPath, caKeyPath)
	proxyArgs := []string{
		"-transparent",
		"-addr", proxyAddr,
		"-rules", rulesTmp,
		"-audit", proxyAuditPath,
		"-mark", fmt.Sprintf("%d", soMark),
		"-ca-cert", caCertPath,
		"-ca-key", caKeyPath,
	}
	proxyCmd, err := startChild(ctx, &children, "sbxproxy", *proxyBin, proxyArgs, nil)
	if err != nil {
		log.Fatalf("start proxy: %v", err)
	}
	if err := waitForListen(ctx, proxyAddr, 5*time.Second); err != nil {
		_ = proxyCmd.Process.Kill()
		log.Fatalf("proxy did not become ready: %v", err)
	}

	// 1b. Install iptables OUTPUT REDIRECT rules so the agent's
	// outbound TCP (in the shared netns) lands on sbxproxy. The mark
	// rule MUST come first so the proxy's own upstream traffic isn't
	// looped back; the loopback rule keeps in-pod localhost traffic
	// (e.g. unit tests, future control sockets) untouched.
	if err := installIptables(ctx, proxyPort, soMark); err != nil {
		log.Fatalf("install iptables: %v", err)
	}
	log.Printf("sandboxd: iptables OUTPUT nat redirect → %s installed (mark=0x%x)", proxyAddr, soMark)

	// 2. sbxfuse — one daemon per fs entry. Each gets its own ACL file,
	// audit log, mount point, and backend dir; remote backends also get
	// their own oplog + uploader inside their own sbxfuse process so a
	// stuck remote on one mount can't block writes to another.
	for i := range sp.FS {
		f := &sp.FS[i]
		aclTmp := filepath.Join(sp.AuditDir, "acls-"+f.Slug()+".json")
		if err := writeACLs(aclTmp, f.ACLs); err != nil {
			log.Fatalf("write ACLs (%s): %v", f.Mount, err)
		}
		fuseArgs := []string{
			"-mount", f.Mount,
			"-backend", f.BackendPath(),
			"-audit", fuseAuditPathFor(sp.AuditDir, f),
			"-acls", aclTmp,
			"-audit-reads",
		}
		// Remote backends: forward the backend name + a JSON-encoded
		// config blob to sbxfuse, which constructs the [remotefs.Store]
		// via a switch and stands up an Oplog. FS.BackendConfigJSON
		// produces the right shape for the active backend (e.g. the
		// fields that map onto [remotefs.GoogleDriveConfig] for gdrive).
		if f.Backend.IsRemote() {
			blob, err := f.BackendConfigJSON()
			if err != nil {
				log.Fatalf("backend %q config (%s): %v", f.Backend, f.Mount, err)
			}
			fuseArgs = append(fuseArgs,
				"-remote", string(f.Backend),
				"-remote-config", string(blob),
				// Same SO_MARK we hand sbxproxy: sbxfuse's outbound TLS to
				// the cloud API needs to escape the OUTPUT REDIRECT too,
				// otherwise its traffic ends up at sbxproxy and gets
				// allowlist-checked against the workload's egress rules.
				"-mark", fmt.Sprintf("%d", soMark),
			)
		}
		fuseCmd, err := startChild(ctx, &children, "sbxfuse:"+f.Slug(), *fuseBin, fuseArgs, nil)
		if err != nil {
			log.Fatalf("start fuse (%s): %v", f.Mount, err)
		}
		// Cloud-bootstrapped backends (gdrive) have to fetch a directory
		// listing before they can mount; sbxfuse caps that bootstrap at
		// 30s. Wait long enough to cover it plus a small buffer — local
		// backends still return in <100ms so this isn't a slowdown.
		if err := waitForMountReady(ctx, f.Mount, mountReadTimout); err != nil {
			_ = fuseCmd.Process.Kill()
			log.Fatalf("fuse did not mount %s: %v", f.Mount, err)
		}
	}

	// 3. Agent. Egress + workspace are now mediated.
	//
	// Per DESIGN.md §3.3, the agent runs in its OWN container, not as a
	// child process of sandboxd. We unpack the agent image's docker-archive
	// tarball into an OCI rootfs, generate a runtime spec, and hand it to
	// runc. The agent container shares the sandbox-pod's network namespace
	// (so the iptables REDIRECT above applies to it too) and bind-mounts
	// /workspace from the FUSE mount.
	//
	// We deliberately do NOT inject HTTP_PROXY / HTTPS_PROXY: the agent
	// makes plain TCP and the kernel transparently redirects it to
	// sbxproxy. No agent cooperation, no language-specific proxy quirks.
	bundleDir, err := os.MkdirTemp("", "agent-bundle-*")
	if err != nil {
		log.Fatalf("create bundle dir: %v", err)
	}
	defer os.RemoveAll(bundleDir)
	rootfsDir := filepath.Join(bundleDir, "rootfs")

	imgCfg, err := runc.ExtractDockerArchive(spec.AgentImageTar, rootfsDir)
	if err != nil {
		log.Fatalf("unpack agent image %s: %v", spec.AgentImageTar, err)
	}
	log.Printf("sandboxd: agent image unpacked to %s (entrypoint=%v)", rootfsDir, imgCfg.Entrypoint)

	// Seed each FUSE backend from the agent image's own rootfs: any
	// content the image carries at the mount path (e.g. `COPY inputs/
	// /workspace/inputs/` in the Dockerfile) gets moved into the
	// backend so the agent sees it through the FUSE mount, with
	// whatever access the ACLs grant. Move (not copy) so we don't
	// keep two copies — runc's bind mount over the mount will hide
	// whatever's left at <rootfs><mount> at runtime anyway.
	for i := range sp.FS {
		f := &sp.FS[i]
		entries, err := os.ReadDir(filepath.Join(rootfsDir, f.Mount))
		if err != nil {
			continue
		}
		for _, e := range entries {
			src := filepath.Join(rootfsDir, f.Mount, e.Name())
			dst := filepath.Join(f.BackendPath(), e.Name())
			if err := os.Rename(src, dst); err != nil {
				log.Fatalf("seed from agent rootfs %s → %s: %v", src, dst, err)
			}
		}
		if len(entries) > 0 {
			log.Printf("sandboxd: seeded %s from agent rootfs %s (%d entries)",
				f.BackendPath(), filepath.Join(rootfsDir, f.Mount), len(entries))
		}
	}

	sandboxd.AddCA(rootfsDir, caCert)

	mounts := make([]runc.BindMount, 0, len(sp.FS)+2)
	for i := range sp.FS {
		mounts = append(mounts, runc.BindMount{
			Source: sp.FS[i].Mount, Destination: sp.FS[i].Mount, Options: []string{"rw"},
		})
	}
	// /etc/hosts and /etc/resolv.conf are needed by the agent so
	// hostnames resolve. With the legacy HTTP_PROXY model the
	// agent dialed 127.0.0.1 and DNS was the proxy's problem;
	// in transparent mode the agent does its own DNS, then the
	// kernel redirects the resulting TCP. The parent's files
	// already carry --add-host entries for upstream-allowed/denied.
	mounts = append(mounts,
		runc.BindMount{Source: "/etc/hosts", Destination: "/etc/hosts", Options: []string{"ro"}},
		runc.BindMount{Source: "/etc/resolv.conf", Destination: "/etc/resolv.conf", Options: []string{"ro"}},
	)

	if err := runc.WriteConfig(runc.BundleParams{
		BundleDir:   bundleDir,
		ImageConfig: imgCfg,
		ExtraEnv:    sp.Agent.Env,
		Hostname:    "agent",
		Mounts:      mounts,
	}); err != nil {
		log.Fatalf("write bundle config: %v", err)
	}

	containerID := fmt.Sprintf("agent-%d", os.Getpid())
	agentCmd, err := startChild(ctx, &children, "agent", "runc",
		[]string{"run", "-b", bundleDir, containerID}, nil)
	if err != nil {
		log.Fatalf("start agent (runc): %v", err)
	}

	go func() {
		_ = agentCmd.Wait()
		log.Println("sandboxd: agent finished, shutting down sidecars")
		cancel()
	}()
	<-ctx.Done()
	children.Wait()
}

// startChild spawns name with the given args/env, prefix-streams its
// stdout/stderr to ours, and tracks completion via wg.
//
// On ctx cancel, the child is given SIGTERM (with a WaitDelay grace period
// before SIGKILL) so subsystems with cleanup hooks get a chance to
// finish: sbxfuse needs to run fusermount -u, and (when a remote
// backend is in play) drain its oplog of pending uploads. The grace
// must outlive the longest cleanup; sbxfuse's oplog drain is bounded
// at 5s, so 10s here gives that drain plus mount teardown room.
func startChild(ctx context.Context, wg *sync.WaitGroup, name, bin string, args, env []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if env != nil {
		cmd.Env = env
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		streamPrefixed(name+":out", stdout)
	}()
	go func() {
		defer wg.Done()
		streamPrefixed(name+":err", stderr)
	}()
	return cmd, nil
}

func streamPrefixed(prefix string, r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			fmt.Fprintf(os.Stderr, "[%s] %s", prefix, line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "[%s] read error: %v\n", prefix, err)
			}
			return
		}
	}
}

func waitForListen(ctx context.Context, addr string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", addr)
}

// fuseSuperMagic is the f_type value Linux reports via statfs for any
// FUSE filesystem (see <linux/magic.h> FUSE_SUPER_MAGIC). We use it to
// distinguish "sbxfuse mounted here" from "this path happens to be a
// regular directory" — the latter is the silent-failure mode where
// sbxfuse hung in init but the mountpoint pre-existed in the rootfs.
const fuseSuperMagic = 0x65735546

func waitForMountReady(ctx context.Context, mp string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mp, &st); err == nil && st.Type == fuseSuperMagic {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for FUSE mount %s (statfs did not report fuse magic — sbxfuse likely failed during init)", mp)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// fuseAuditPathFor returns the per-mount audit log path. Each sbxfuse
// process gets its own file so concurrent appends don't interleave.
func fuseAuditPathFor(auditDir string, f *spec.FS) string {
	return filepath.Join(auditDir, "fuse-"+f.Slug()+".log")
}

// writeACLs serializes the ACL list to a file sbxfuse can load.
func writeACLs(path string, acls any) error {
	return writeJSON(path, acls)
}

// writeJSON encodes v to path as a single JSON document.
func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(v)
}

// installIptables sets up the OUTPUT-chain nat REDIRECT rules that make
// the proxy transparent to the agent. Runs in the sandbox-pod's network
// namespace, so the agent (which shares the netns) inherits these rules.
//
// Rule order matters:
//  1. -m mark --mark <soMark> -j RETURN — proxy's upstream traffic is
//     stamped with SO_MARK by the dialer in internal/proxy/mark_linux.go
//     and must escape the redirect, otherwise the proxy talks to itself.
//  2. -o lo -j RETURN — keep loopback traffic untouched (e.g. the proxy's
//     own listener, future control sockets).
//  3. -p tcp -j REDIRECT --to-ports <proxyPort> — everything else.
//
// Requires CAP_NET_ADMIN; the sandbox-pod runs --privileged for now.
func installIptables(ctx context.Context, proxyPort, soMark int) error {
	rules := [][]string{
		{"-t", "nat", "-A", "OUTPUT", "-m", "mark", "--mark", fmt.Sprintf("0x%x", soMark), "-j", "RETURN"},
		{"-t", "nat", "-A", "OUTPUT", "-o", "lo", "-j", "RETURN"},
		{"-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-j", "REDIRECT", "--to-ports", fmt.Sprintf("%d", proxyPort)},
	}
	for _, args := range rules {
		out, err := exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("iptables %v: %w (%s)", args, err, bytes.TrimSpace(out))
		}
	}
	return nil
}

// tailAudit follows a sidecar's JSON-line audit file and re-emits each
// event on sandboxd's own log stream as a single "agent op | …" line.
//
// Standard tail-follow pattern: open the file (creating it if the sidecar
// hasn't yet), read until EOF, sleep briefly, repeat. The audit files are
// append-only so seeking forward by the bytes-consumed is safe.
func tailAudit(ctx context.Context, path string, format func(map[string]any) string) {
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	for ctx.Err() == nil {
		if f == nil {
			ff, err := os.Open(path)
			if err != nil {
				if !sleepCtx(ctx, 100*time.Millisecond) {
					return
				}
				continue
			}
			f = ff
		}
		var leftover []byte
		buf := make([]byte, 4096)
	read:
		for {
			n, err := f.Read(buf)
			if n > 0 {
				leftover = append(leftover, buf[:n]...)
				for {
					i := bytes.IndexByte(leftover, '\n')
					if i < 0 {
						break
					}
					line := leftover[:i]
					leftover = leftover[i+1:]
					var ev map[string]any
					if json.Unmarshal(line, &ev) == nil {
						log.Printf("sandboxd: agent op | %s", format(ev))
					}
				}
			}
			if errors.Is(err, io.EOF) {
				if !sleepCtx(ctx, 100*time.Millisecond) {
					return
				}
				continue read
			}
			if err != nil {
				return
			}
		}
	}
}

// sleepCtx returns false if ctx is cancelled before d elapses.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// formatProxyEvent renders an internal/proxy.AuditEvent map for the
// "agent op | …" log line. Schema (see internal/proxy.AuditEvent):
//
//	{at, type:"network", method, host, path, verdict}
func formatProxyEvent(ev map[string]any) string {
	verdict, _ := ev["verdict"].(string)
	method, _ := ev["method"].(string)
	host, _ := ev["host"].(string)
	path, _ := ev["path"].(string)
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("proxy %-5s %s %s%s", verdict, method, host, path)
}

// formatFuseEvent renders an internal/fusefs.AuditEvent map. Schema:
//
//	{at, type:"filesystem", op, path, verdict, err?}
func formatFuseEvent(ev map[string]any) string {
	verdict, _ := ev["verdict"].(string)
	op, _ := ev["op"].(string)
	path, _ := ev["path"].(string)
	return fmt.Sprintf("fuse  %-5s %-10s %s", verdict, op, path)
}
