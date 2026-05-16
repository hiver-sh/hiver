// Command sandboxd is the runtime agent that wires together the
// MITM proxy, FUSE daemon, and agent workload as a single sandbox "pod".
//
// sandboxd is configured by a single JSON spec file (see internal/spec).
// The spec carries everything sandboxd needs: the agent binary + args, the
// workspace's host-side backend and FUSE mount point, the FUSE ACLs, the
// proxy's egress allowlist, and where to write audit logs.
//
// Scope (T47, T50): launch the three processes in the right order
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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sandbox-platform/agent-sandbox/internal/api"
	"github.com/sandbox-platform/agent-sandbox/internal/events"
	"github.com/sandbox-platform/agent-sandbox/internal/netmark"
	"github.com/sandbox-platform/agent-sandbox/internal/runc"
	"github.com/sandbox-platform/agent-sandbox/internal/sandboxd"
	"github.com/sandbox-platform/agent-sandbox/internal/spec"

	gen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/sandbox"
)

const (
	defaultTtl = 1800 * time.Second

	mountReadTimout = 35 * time.Second

	// drainTimeout caps how long we'll wait for /v1/events subscribers
	// to consume trailing events after the agent exits; drainQuietFor
	// is the publish-quiet window the broker must see before declaring
	// itself drained, sized to cover the sidecar→translator hop for
	// last-moment audit events.
	drainTimeout  = 5 * time.Second
	drainQuietFor = 500 * time.Millisecond

	// httpShutdownTimeout caps how long http.Server.Shutdown will
	// wait for SSE handlers (and any other in-flight requests) to
	// return after the broker has been closed. Generous because the
	// kernel needs a moment to flush the trailing bytes + FIN to
	// every subscriber over the docker bridge.
	httpShutdownTimeout = 3 * time.Second
)

func main() {
	var (
		specPath      = flag.String("spec", "", "path to the sandbox spec JSON (required)")
		proxyBin      = flag.String("proxy-bin", "sbxproxy", "path to sbxproxy binary")
		fuseBin       = flag.String("fuse-bin", "sbxfuse", "path to sbxfuse binary")
		apiServerPort = flag.String("api-server-port", "8080", "port of the API server")
		workDir       = flag.String("work-dir", "/run/sandboxd", "directory for sandboxd's internal scratch (CA cert, rules JSON, ACL files, persisted config)")
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

	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		log.Fatalf("create work dir %s: %v", *workDir, err)
	}
	log.Printf("sandboxd: work dir = %s", *workDir)

	// Persist the parsed spec as the API's source-of-truth config so
	// GET/PUT /v1/config have something to read/diff against from the
	// first request. The on-disk format is gen.SandboxConfig JSON; the
	// spec's JSON shape already matches, so we round-trip through the
	// generated type for type safety.
	configPath := filepath.Join(*workDir, "config.json")
	if err := writeInitialConfig(configPath, sp); err != nil {
		log.Fatalf("write initial config %s: %v", configPath, err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// The broker is the single fan-out point for the SSE `/v1/events`
	// stream. Sidecar audit events arrive over the per-child socketpair
	// (see startSidecar), get translated to SandboxEvent shape, and
	// Publish'd here; api.NewSandboxServer hands subscribers to the SSE handler.
	broker := events.New(events.DefaultCapacity, 0)
	store := api.NewConfigStore(configPath)

	// Lifetime expires the sandbox if /v1/ping isn't called within
	// SandboxConfig.Ttl seconds. ttlFn samples the current config on
	// every tick so a /v1/config update changes the deadline without
	// a restart; nil/0 disables the check, matching configs that omit
	// Ttl. On expiry we cancel the lifecycle context — same shutdown
	// path SIGTERM takes.
	lifetime := api.NewLifetime(
		func() time.Duration {
			cfg, err := store.Get()
			if err != nil || cfg.Ttl == nil {
				return defaultTtl
			}
			return time.Duration(*cfg.Ttl) * time.Second
		},
		func() {
			log.Println("sandboxd: TTL elapsed since last /v1/ping, shutting down")
			cancel()
		},
	)
	go lifetime.Run(ctx)

	for i := range sp.FS {
		f := &sp.FS[i]
		if err := os.MkdirAll(f.Mount, 0o755); err != nil {
			log.Fatalf("create mount point %s: %v", f.Mount, err)
		}
		if err := os.MkdirAll(f.BackendPath(), 0o755); err != nil {
			log.Fatalf("create backend %s: %v", f.BackendPath(), err)
		}
	}

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

	var children sync.WaitGroup

	// 1. sbxproxy in transparent mode. iptables (set up below) will
	// REDIRECT all outbound TCP from agent processes here; sbxproxy
	// recovers the original destination via SO_ORIGINAL_DST and
	// dispatches by protocol sniff. The agent itself is unaware of
	// the proxy — no HTTP_PROXY env, no opt-in cooperation required.
	rulesTmp := filepath.Join(*workDir, "egress-rules.json")
	if err := writeJSON(rulesTmp, sp.Egress.Allow); err != nil {
		log.Fatalf("write rules: %v", err)
	}

	caCertPath := filepath.Join(*workDir, "sandbox-ca.crt")
	caKeyPath := filepath.Join(*workDir, "sandbox-ca.key")

	caCert := sandboxd.GenerateCaCert(caCertPath, caKeyPath)
	proxyArgs := []string{
		"-transparent",
		"-addr", proxyAddr,
		"-rules", rulesTmp,
		"-mark", fmt.Sprintf("%d", soMark),
		"-ca-cert", caCertPath,
		"-ca-key", caKeyPath,
	}
	proxyCmd, err := startSidecar(ctx, &children, "sbxproxy", *proxyBin, proxyArgs, nil,
		sidecarOnEvent(formatProxyEvent, newProxyTranslator(broker).handle))
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
	fsSidecars := map[string]fsSidecar{}
	for i := range sp.FS {
		f := &sp.FS[i]
		aclTmp := filepath.Join(*workDir, "acls-"+f.Slug()+".json")
		if err := writeACLs(aclTmp, f.ACLs); err != nil {
			log.Fatalf("write ACLs (%s): %v", f.Mount, err)
		}
		fuseArgs := []string{
			"-mount", f.Mount,
			"-backend", f.BackendPath(),
			"-acls", aclTmp,
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
		fuseCmd, err := startSidecar(ctx, &children, "sbxfuse:"+f.Slug(), *fuseBin, fuseArgs, nil,
			sidecarOnEvent(formatFuseEvent, newFuseTranslator(broker, f.Mount, gen.Backend(f.Backend)).handle))
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
		fsSidecars[f.Mount] = fsSidecar{pid: fuseCmd.Process.Pid, aclPath: aclTmp}
	}

	// Reconcile sidecar policy whenever the API publishes a
	// config.apply event: re-derive egress rules + per-mount ACLs from
	// the persisted config, rewrite the files each sidecar reads on
	// SIGHUP, and signal. Sidecars keep the current policy on read
	// errors so a half-written file can't relax access by accident.
	go reconcileSidecars(ctx, broker, proxyCmd.Process.Pid, configPath, rulesTmp, fsSidecars)

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

	imgCfg, err := runc.ExtractDockerArchive(spec.SandboxImageTar, rootfsDir)
	if err != nil {
		log.Fatalf("unpack agent image %s: %v", spec.SandboxImageTar, err)
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
		ExtraEnv:    sp.Env,
		Hostname:    "agent",
		Mounts:      mounts,
	}); err != nil {
		log.Fatalf("write bundle config: %v", err)
	}

	containerID := fmt.Sprintf("agent-%d", os.Getpid())
	agentCmd, agentStdioDone, err := startChild(ctx, &children, "agent", "runc",
		[]string{"run", "-b", bundleDir, containerID}, nil, nil,
		publishAgentStdio(broker))
	if err != nil {
		log.Fatalf("start agent (runc): %v", err)
	}

	// Start Sandbox API server. /v1/sandbox proxies to the agent's
	// exposed port over loopback; the marked dialer keeps that traffic
	// out of the iptables OUTPUT REDIRECT (otherwise it would loop
	// through sbxproxy and get allowlist-checked against the
	// workload's egress rules).
	proxyTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			Control: netmark.Control(soMark),
		}).DialContext,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	s := api.NewSandboxServer(
		*apiServerPort,
		imgCfg.ExposedPort,
		proxyTransport,
		broker,
		store,
		lifetime)

	go s.ListenAndServe()

	// Graceful shutdown chain triggered by the agent exiting:
	//   1. wait for the audit pipeline to settle and SSE subscribers
	//      to consume trailing events (drain)
	//   2. close the broker, which closes every subscriber channel and
	//      lets the SSE handlers fall through their receive loop
	//   3. http.Server.Shutdown, which waits for those handlers (and
	//      any other in-flight requests) to return — that's the only
	//      point at which the kernel has actually sent the trailing
	//      SSE bytes and FIN'd the TCP connection cleanly
	//   4. cancel the lifecycle ctx, which SIGTERMs the sidecars
	go func() {
		<-agentStdioDone
		_ = agentCmd.Wait()
		log.Println("sandboxd: agent finished")
		if n := broker.SubscriberCount(); n > 0 {
			log.Printf("sandboxd: waiting for %d event subscriber(s) to drain", n)
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
			if err := broker.WaitDrained(drainCtx, drainQuietFor); err != nil {
				log.Printf("sandboxd: event drain timed out: %v", err)
			}
			cancelDrain()
		}
		broker.Close()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), httpShutdownTimeout)
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Printf("sandboxd: http shutdown: %v", err)
		}
		cancelShutdown()
		log.Println("sandboxd: shutting down sidecars")
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
// onStdio (when non-nil) is invoked per line of child output, tagged
// "stdout" or "stderr". Used by the agent spawn to publish StdioEvents
// into the broker; sidecars pass nil because their stdout is
// operational logging, not workload output.
//
// stdioDone closes once both the stdout and stderr pipe readers have
// seen EOF (i.e. every onStdio call for this child has returned).
// cmd.Wait returns when the *process* exits — which can be before the
// kernel pipe has been fully drained — so callers that need to be sure
// every line was processed (the agent flow has to publish trailing
// stdio events before closing the broker) must wait on stdioDone in
// addition to cmd.Wait().
func startChild(ctx context.Context, wg *sync.WaitGroup, name, bin string, args, env []string, extraFiles []*os.File, onStdio func(stream, line string)) (cmd *exec.Cmd, stdioDone <-chan struct{}, err error) {
	cmd = exec.CommandContext(ctx, bin, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if env != nil {
		cmd.Env = env
	}
	// ExtraFiles[i] becomes fd 3+i in the child. Used by startSidecar
	// to hand the child its events-stream fd (3).
	if len(extraFiles) > 0 {
		cmd.ExtraFiles = extraFiles
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	var outCb, errCb func(string)
	if onStdio != nil {
		outCb = func(line string) { onStdio("stdout", line) }
		errCb = func(line string) { onStdio("stderr", line) }
	}
	done := make(chan struct{})
	var stdioWg sync.WaitGroup
	stdioWg.Add(2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer stdioWg.Done()
		streamPrefixed(name+":out", stdout, outCb)
	}()
	go func() {
		defer wg.Done()
		defer stdioWg.Done()
		streamPrefixed(name+":err", stderr, errCb)
	}()
	go func() {
		stdioWg.Wait()
		close(done)
	}()
	return cmd, done, nil
}

func streamPrefixed(prefix string, r io.Reader, onLine func(string)) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			fmt.Fprintf(os.Stderr, "[%s] %s", prefix, line)
			if onLine != nil {
				onLine(line)
			}
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

// fsSidecar identifies one sbxfuse process for the reconciler: the pid
// we SIGHUP and the ACL file we rewrite for it.
type fsSidecar struct {
	pid     int
	aclPath string
}

// reconcileSidecars subscribes to the broker and, for every successful
// config.apply event, rewrites the on-disk policy files each sidecar
// reads (sbxproxy's egress allowlist + each sbxfuse's per-mount ACLs)
// and signals them with SIGHUP. We re-read the full config from disk
// (rather than re-applying just the event's `changes` diff) so a
// missed or out-of-order event can't leave a sidecar stuck on a stale
// policy.
//
// fsSidecars is keyed by agent-visible mount path; FS entries in the
// new config that don't match an existing sidecar are ignored — adding
// or removing mounts at runtime isn't supported, and we lean
// conservative ("don't quietly change something we can't enforce")
// rather than try to start/stop sbxfuse processes mid-flight.
func reconcileSidecars(ctx context.Context, broker *events.Broker, proxyPID int, configPath, rulesPath string, fsSidecars map[string]fsSidecar) {
	ch, cancel := broker.Subscribe(0)
	defer cancel()
	for {
		select {
		case entry, ok := <-ch:
			if !ok {
				return
			}
			disc, err := entry.Event.Discriminator()
			if err != nil || disc != "config.apply" {
				continue
			}
			ev, err := entry.Event.AsConfigApplyEvent()
			if err != nil || !ev.Success {
				continue
			}
			cfg, err := readPersistedConfig(configPath)
			if err != nil {
				log.Printf("sandboxd: reconcile: %v", err)
				continue
			}
			if err := writeEgressRules(rulesPath, cfg); err != nil {
				log.Printf("sandboxd: reconcile egress: %v", err)
			} else if err := syscall.Kill(proxyPID, syscall.SIGHUP); err != nil {
				log.Printf("sandboxd: SIGHUP sbxproxy (pid=%d): %v", proxyPID, err)
			}
			for _, fs := range cfg.Fs {
				base := api.FSBase(fs)
				sc, ok := fsSidecars[base.Mount]
				if !ok {
					log.Printf("sandboxd: reconcile fs: no sidecar for mount %q (mount add/remove not supported)", base.Mount)
					continue
				}
				if err := writeACLsForMount(sc.aclPath, fs); err != nil {
					log.Printf("sandboxd: reconcile acls (%s): %v", base.Mount, err)
					continue
				}
				if err := syscall.Kill(sc.pid, syscall.SIGHUP); err != nil {
					log.Printf("sandboxd: SIGHUP sbxfuse (mount=%s pid=%d): %v", base.Mount, sc.pid, err)
					continue
				}
			}
			log.Printf("sandboxd: reconciled sidecar policy from config (event id=%d)", entry.ID)
		case <-ctx.Done():
			return
		}
	}
}

// readPersistedConfig reads the on-disk SandboxConfig JSON the API
// server writes through.
func readPersistedConfig(path string) (gen.SandboxConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return gen.SandboxConfig{}, fmt.Errorf("read config: %w", err)
	}
	var cfg gen.SandboxConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return gen.SandboxConfig{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// writeEgressRules writes cfg.Egress.Allow (or `[]` if absent) to
// rulesPath in the JSON shape sbxproxy expects. gen.EgressRule and
// proxy.EgressRule share their wire format, so no conversion is
// needed — sbxproxy unmarshals into its own type.
func writeEgressRules(rulesPath string, cfg gen.SandboxConfig) error {
	allow := []gen.EgressRule{}
	if cfg.Egress != nil && cfg.Egress.Allow != nil {
		allow = *cfg.Egress.Allow
	}
	return writeJSON(rulesPath, allow)
}

// writeACLsForMount writes fs.Acls to aclPath in the JSON shape
// sbxfuse expects. gen.ACLRule and fusefs.Rule share their wire
// format.
func writeACLsForMount(aclPath string, fs gen.FileSystem) error {
	acls := []gen.ACLRule{}
	if a := api.FSBase(fs).Acls; a != nil {
		acls = *a
	}
	return writeJSON(aclPath, acls)
}

// writeInitialConfig persists sp to path in the gen.SandboxConfig JSON
// shape that GET /v1/config serves. spec.Spec's JSON tags align with
// gen.SandboxConfig's, so a marshal/unmarshal round-trip is enough —
// going through the generated type catches drift between the two
// structs at startup rather than at first GET.
func writeInitialConfig(path string, sp *spec.Spec) error {
	data, err := json.Marshal(sp)
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}
	var cfg gen.SandboxConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("unmarshal as SandboxConfig: %w", err)
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// installIptables sets up the OUTPUT-chain nat REDIRECT rules that make
// the proxy transparent to the agent. Runs in the sandbox-pod's network
// namespace, so the agent (which shares the netns) inherits these rules.
//
// Loopback is NOT exempted: an in-pod listener like sandboxd's own API
// server at 127.0.0.1:8080 is just another egress target, and the agent
// must be subject to the same allowlist for it as for the public
// internet. Sandboxd's own readiness-probe dial to the proxy address
// works because the proxy's transparent handler detects "SO_ORIGINAL_DST
// equals LocalAddr" (no real NAT happened) and bails out early.
//
// Rule order:
//  1. -m mark --mark <soMark> -j RETURN — proxy- and sbxfuse-originated
//     upstream traffic is stamped with SO_MARK so it escapes the
//     redirect, otherwise the proxy talks to itself.
//  2. -p tcp -j REDIRECT --to-ports <proxyPort> — everything else,
//     loopback included.
//
// Requires CAP_NET_ADMIN; the sandbox-pod runs --privileged for now.
func installIptables(ctx context.Context, proxyPort, soMark int) error {
	rules := [][]string{
		{"-t", "nat", "-A", "OUTPUT", "-m", "mark", "--mark", fmt.Sprintf("0x%x", soMark), "-j", "RETURN"},
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
