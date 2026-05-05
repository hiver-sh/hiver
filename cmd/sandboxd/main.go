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

	"github.com/sandbox-platform/agent-sandbox/internal/proxy"
	"github.com/sandbox-platform/agent-sandbox/internal/runc"
	"github.com/sandbox-platform/agent-sandbox/internal/spec"
)

func main() {
	var (
		specPath = flag.String("spec", "", "path to the sandbox spec JSON (required)")
		proxyBin = flag.String("proxy-bin", "sbxproxy", "path to sbxproxy binary")
		fuseBin  = flag.String("fuse-bin", "sbxfuse", "path to sbxfuse binary")
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

	if err := os.MkdirAll(sp.AuditDir, 0o755); err != nil {
		log.Fatalf("create audit dir %s: %v", sp.AuditDir, err)
	}
	log.Printf("sandboxd: audit dir = %s", sp.AuditDir)

	if err := os.MkdirAll(sp.FS.Mount, 0o755); err != nil {
		log.Fatalf("create mount point %s: %v", sp.FS.Mount, err)
	}
	backendPath := sp.FS.Backend.HostPath()
	if err := os.MkdirAll(backendPath, 0o755); err != nil {
		log.Fatalf("create backend %s: %v", backendPath, err)
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
	fuseAuditPath := filepath.Join(sp.AuditDir, "fuse.log")

	// As the orchestrator, sandboxd is responsible for reporting what
	// the agent does — the agent itself produces no application-level
	// logs. We tail the proxy and FUSE audit streams (the canonical
	// per-operation record per DESIGN.md §9.1) and surface each event
	// as a single "agent op | …" line on sandboxd's stdout.
	go tailAudit(ctx, proxyAuditPath, formatProxyEvent)
	go tailAudit(ctx, fuseAuditPath, formatFuseEvent)

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
	// Generate a per-pod CA. sbxproxy uses it to mint leaf certs for
	// TLS interception (any rule with paths/methods/headers triggers
	// termination). The cert PEM is also spliced into the agent rootfs
	// trust store below so the agent's TLS handshake validates the
	// proxy-presented leaf.
	caCert, caKey, err := proxy.GenerateCA()
	if err != nil {
		log.Fatalf("generate CA: %v", err)
	}
	caCertPath := filepath.Join(sp.AuditDir, "sandbox-ca.crt")
	caKeyPath := filepath.Join(sp.AuditDir, "sandbox-ca.key")
	if err := os.WriteFile(caCertPath, proxy.EncodeCertPEM(caCert), 0o644); err != nil {
		log.Fatalf("write ca cert: %v", err)
	}
	caKeyPEM, err := proxy.EncodeKeyPEM(caKey)
	if err != nil {
		log.Fatalf("encode ca key: %v", err)
	}
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0o600); err != nil {
		log.Fatalf("write ca key: %v", err)
	}
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

	// 2. sbxfuse. Pass the ACLs by writing them to a file the daemon reads.
	aclTmp := filepath.Join(sp.AuditDir, "acls.json")
	if err := writeACLs(aclTmp, sp.FS.ACLs); err != nil {
		log.Fatalf("write ACLs: %v", err)
	}
	fuseArgs := []string{
		"-mount", sp.FS.Mount,
		"-backend", backendPath,
		"-audit", fuseAuditPath,
		"-acls", aclTmp,
	}
	if sp.FS.AuditReads {
		fuseArgs = append(fuseArgs, "-audit-reads")
	}
	// Remote backends: forward the backend name + a JSON-encoded
	// config blob to sbxfuse, which constructs the [remotefs.Store]
	// via a switch and stands up an Oplog. spec.FS.BackendConfigJSON
	// produces the right shape for the active backend (e.g. the
	// fields that map onto [remotefs.GoogleDriveConfig] for gdrive).
	if sp.FS.Backend.IsRemote() {
		blob, err := sp.FS.BackendConfigJSON()
		if err != nil {
			log.Fatalf("backend %q config: %v", sp.FS.Backend, err)
		}
		fuseArgs = append(fuseArgs,
			"-remote", string(sp.FS.Backend),
			"-remote-config", string(blob),
		)
	}
	fuseCmd, err := startChild(ctx, &children, "sbxfuse", *fuseBin, fuseArgs, nil)
	if err != nil {
		log.Fatalf("start fuse: %v", err)
	}
	if err := waitForMountReady(ctx, sp.FS.Mount, 5*time.Second); err != nil {
		_ = fuseCmd.Process.Kill()
		log.Fatalf("fuse did not mount: %v", err)
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

	// Seed the FUSE backend from the agent image's own rootfs: any
	// content the image carries at fs.mount (e.g. `COPY inputs/
	// /workspace/inputs/` in the Dockerfile) gets moved into the
	// backend so the agent sees it through the FUSE mount, with
	// whatever access the ACLs grant. Move (not copy) so we don't
	// keep two copies — runc's bind mount over fs.mount will hide
	// whatever's left at <rootfs><mount> at runtime anyway.
	if entries, err := os.ReadDir(filepath.Join(rootfsDir, sp.FS.Mount)); err == nil {
		for _, e := range entries {
			src := filepath.Join(rootfsDir, sp.FS.Mount, e.Name())
			dst := filepath.Join(backendPath, e.Name())
			if err := os.Rename(src, dst); err != nil {
				log.Fatalf("seed from agent rootfs %s → %s: %v", src, dst, err)
			}
		}
		if len(entries) > 0 {
			log.Printf("sandboxd: seeded %s from agent rootfs %s (%d entries)",
				backendPath, filepath.Join(rootfsDir, sp.FS.Mount), len(entries))
		}
	}

	// Splice the per-pod CA into the agent rootfs trust store so the
	// agent's TLS handshakes validate the leaf certs sbxproxy mints
	// during interception. We append (not replace) so the agent keeps
	// trust for unintercepted TLS to public hosts. Best effort: an
	// image without ca-certificates installed gets a warning — its TLS
	// requests will fail anyway, with or without our addition.
	caBundle := filepath.Join(rootfsDir, "etc/ssl/certs/ca-certificates.crt")
	if existing, err := os.ReadFile(caBundle); err == nil {
		merged := append(existing, '\n')
		merged = append(merged, proxy.EncodeCertPEM(caCert)...)
		if err := os.WriteFile(caBundle, merged, 0o644); err != nil {
			log.Fatalf("install sandbox CA into agent rootfs: %v", err)
		}
		log.Printf("sandboxd: installed sandbox CA into %s", caBundle)
	} else {
		log.Printf("sandboxd: agent rootfs has no %s — TLS interception will fail; install ca-certificates in the agent image", caBundle)
	}

	extraEnv := append([]string{
		"WORKSPACE=" + sp.FS.Mount,
	}, sp.Agent.Env...)

	if err := runc.WriteConfig(runc.BundleParams{
		BundleDir:   bundleDir,
		ImageConfig: imgCfg,
		ExtraEnv:    extraEnv,
		Hostname:    "agent",
		Mounts: []runc.BindMount{
			{Source: sp.FS.Mount, Destination: sp.FS.Mount, Options: []string{"rw"}},
			// /etc/hosts and /etc/resolv.conf are needed by the agent so
			// hostnames resolve. With the legacy HTTP_PROXY model the
			// agent dialed 127.0.0.1 and DNS was the proxy's problem;
			// in transparent mode the agent does its own DNS, then the
			// kernel redirects the resulting TCP. The parent's files
			// already carry --add-host entries for upstream-allowed/denied.
			{Source: "/etc/hosts", Destination: "/etc/hosts", Options: []string{"ro"}},
			{Source: "/etc/resolv.conf", Destination: "/etc/resolv.conf", Options: []string{"ro"}},
		},
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
// before SIGKILL) so subsystems with cleanup hooks — notably sbxfuse —
// get a chance to run fusermount -u before exiting.
func startChild(ctx context.Context, wg *sync.WaitGroup, name, bin string, args, env []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
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

func waitForMountReady(ctx context.Context, mp string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, err := os.Stat(mp)
		if err == nil && st.IsDir() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for FUSE mount %s", mp)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
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
