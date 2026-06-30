// Command sandboxd is the runtime agent that wires together the MITM proxy,
// FUSE daemon, and agent workloads as a sandbox "pod".
//
// sandboxd boots as a pack host: it brings up the shared sidecars (sbxproxy,
// sbxfuse) and parks. Each sandbox is created on demand via POST /v1/<key>,
// whose body is the JSON config (see internal/spec) carrying everything that
// sandbox needs: the agent binary + args, the workspace's host-side backend and
// FUSE mount point, the FUSE ACLs, the proxy's egress allowlist, and where to
// write audit logs.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hiver-sh/hiver/internal/api"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/proxy"
	"github.com/hiver-sh/hiver/internal/pty"
	"github.com/hiver-sh/hiver/internal/runc"
	"github.com/hiver-sh/hiver/internal/sandboxd"
	"github.com/hiver-sh/hiver/internal/snapshot"
	"github.com/hiver-sh/hiver/internal/spec"
)

const (
	defaultTtl = 1800 * time.Second

	mountReadTimout = 35 * time.Second

	// eventDrainTimeout caps how long we'll wait for /v1/events subscribers
	// to consume trailing events after the agent exits; drainQuietFor
	// is the publish-quiet window the broker must see before declaring
	// itself drained, sized to cover the sidecar→translator hop for
	// last-moment audit events.
	eventDrainTimeout = 5 * time.Second
	drainQuietFor     = 500 * time.Millisecond

	// httpShutdownTimeout caps how long http.Server.Shutdown will
	// wait for SSE handlers (and any other in-flight requests) to
	// return after the broker has been closed. Generous because the
	// kernel needs a moment to flush the trailing bytes + FIN to
	// every subscriber over the docker bridge.
	httpShutdownTimeout = 3 * time.Second

	// fsDrainTimeout caps how long we'll wait for the workload filesystem
	// flush (microvm guest `sync`) on shutdown before stopping the workload
	// anyway, so a wedged guest can't block teardown past the controller's
	// shutdown grace period.
	fsDrainTimeout = 10 * time.Second

	// snapshotResumeTimeout caps the microvm VM-resume fast path's VMM-up +
	// snapshot-load + resume; on timeout we abort rather than hang the launch.
	snapshotResumeTimeout = 30 * time.Second

	// soMark stamps proxy-originated upstream sockets so the iptables REDIRECT
	// rule can skip them (-m mark) and avoid an infinite loop; it also tags the
	// API server's reverse-proxy dialer. Any non-zero value works — we pick a
	// distinctive one for grep-ability.
	soMark = 0x5b1

	proxyBin = "sbxproxy"
	fuseBin  = "sbxfuse"
	workDir  = "/run/sandboxd"
)

func main() {
	// If this process was re-executed as the microvm namespace-launch helper, enter
	// the requested cgroup/netns/mount-ns, bind the per-VM overlay, and exec the VMM
	// in place — a single fork in place of the old sh→ip netns exec→unshare→sh chain.
	// Returns immediately on a normal sandboxd start; never returns on the helper path
	// (it execs or fatals). Must precede flag parsing — its argv isn't the flag set.
	isolation.MaybeRunNSExec()

	phase := &bootPhase{last: time.Now()}
	var (
		apiServerPort         = flag.String("api-server-port", "8099", "port of the API server")
		pack                  = flag.Bool("pack", false, "run as a multi-tenant pack host: park and serve N same-image sandboxes created on demand via POST /v1/<key>, outliving any single sandbox. When omitted, sandboxd hosts a single sandbox and its own lifecycle follows that sandbox's — the process exits once the sandbox is shut down or killed.")
		snapshotDir           = flag.String("snapshot-dir", "", "directory where files and VM-state snapshots are stored on local disk (skip the pod overlay — point it at NVMe); optional — when unset, files snapshots only work for configs that route them to a FUSE drive via snapshot.files.mount, and VM snapshots are disabled")
		preallocPool          = flag.Int("prealloc-pool", 10, "number of sandbox network slots (netns/veth/iptables + DNS sink) to preallocate and keep warm so a create claims one instead of wiring it on the request path; 0 disables")
		maxConcurrentLaunches = flag.Int("max-concurrent-launches", 10, "cap on concurrent sandbox creates in flight, so a burst doesn't oversubscribe the node's cores during the CPU-bound resume/convergence phases; set near the node's vCPU count; 0 disables (unbounded)")
	)
	flag.Parse()

	// Construct the API server and start serving immediately — before any
	// subsystem exists — so the sandbox binds its port the instant the process
	// starts, rather than after the multi-second proxy/FUSE/image/agent boot.
	// Its dependencies are injected via the SetX methods below as boot creates
	// them; until all are wired the server answers every request with 500.
	// The supervisor owns the pod's sandbox map and is what the API server
	// dispatches keyed requests to. It starts empty; the boot sandbox is created
	// and registered below once its key and subsystems are known.
	sup := newSupervisor()
	s := api.NewSandboxServer(*apiServerPort, sup)
	go s.ListenAndServe()

	if err := os.MkdirAll(workDir, 0o755); err != nil {
		log.Fatalf("create work dir %s: %v", workDir, err)
	}
	log.Printf("sandboxd: work dir = %s", workDir)

	// Docker sets the container hostname to the container's short ID, which
	// is unique per sandbox. os.Getpid() is always 1 in the pod's PID
	// namespace and cannot distinguish sandboxes sharing a host, so the
	// hostname (not the pid) seeds the isolation backend's cgroup path.
	podHostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("get hostname: %v", err)
	}
	// The isolation backend abstracts the runtime boundary (container/runc
	// vs. microvm/firecracker): overlayfs + FUSE assembly, egress firewall
	// rules, the cgroup, and exec/launch all route through it. The backend is
	// detected from the image — a microvm image ships a guest rootfs — not from
	// any config field; Detect errors with a user-friendly message when a
	// microvm image lands on a host without KVM.
	// Each packed sandbox builds its own isolation backend (see createPacked); the
	// pod only needs the detected kind here to hand to the supervisor. The backend
	// is detected from the image, not from any config field.
	isoKind, err := isolation.Detect()
	if err != nil {
		log.Fatalf("isolation: %v", err)
	}
	log.Printf("sandboxd: isolation = %s", isoKind)
	phase.mark("isolation init")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// When sandboxd is the pod's init (PID 1), reap orphaned zombies. A sandbox
	// entrypoint whose runc launcher detached, or an exec grandchild, reparents to
	// init when its parent exits; without reaping, each lingers as a <defunct>
	// zombie holding a PID slot. No-op unless PID 1, and it never races sandboxd's
	// own cmd.Wait() (see reapOrphans).
	go reapOrphans(ctx)

	// The FUSE workspace sidecars run on their own context, not the lifecycle
	// ctx. Shutdown begins by cancelling the lifecycle ctx (signal/TTL), which
	// stops the workload; only after the workload drains does finalizeShutdown
	// capture the snapshot — and a snapshot routed through a FUSE drive
	// (snapshot.mount) must be written while that mount's daemon is still up.
	// Tying the FUSE sidecars to the lifecycle ctx would tear them down at the
	// very start of shutdown, before the capture runs, so the tarball would land
	// on an unmounted (plain) directory and never reach the remote backend.
	// finalizeShutdown cancels this after the capture (see teardownFS).
	fsCtx, cancelFS := context.WithCancel(context.Background())
	defer cancelFS()

	proxyPort, err := freePort()
	if err != nil {
		log.Fatalf("free port: %v", err)
	}
	// Each packed sandbox reaches the proxy/sink via its veth gateway IP (the host
	// DNATs to it), so they must bind all interfaces.
	bindHost := "0.0.0.0"
	proxyAddr := fmt.Sprintf("%s:%d", bindHost, proxyPort)
	// DNS sinkhole port: iptables redirects all workload DNS here and sbxproxy
	// answers with a constant placeholder, so DNS can't be an exfil channel.
	dnsPort, err := freePort()
	if err != nil {
		log.Fatalf("free dns port: %v", err)
	}
	dnsAddr := fmt.Sprintf("%s:%d", bindHost, dnsPort)

	var children sync.WaitGroup

	// sbxproxy (egress) and sbxfuse (workspace) are independent subsystems whose
	// startup is dominated by waiting for each child to boot — the proxy to
	// listen, the FUSE daemons to mount. Bring them up concurrently so those
	// waits overlap rather than sum; both must be ready before the agent launches
	// below. The handful of isolation-backend mutations they make (RedirectEgress,
	// ExportWorkspace) are serialized via isoMu, since the backend isn't
	// guaranteed concurrency-safe — only the boot waits actually overlap.
	rulesTmp := filepath.Join(workDir, "egress-rules.json")
	caCertPath := filepath.Join(workDir, "sandbox-ca.crt")
	caKeyPath := filepath.Join(workDir, "sandbox-ca.key")
	// Coordinates coalesced egress-rule reloads (pack mode). Created here so the
	// proxy event handler below can advance its applied generation from sbxproxy's
	// echo; packState (built later, in the pack branch) drains it via the reloader.
	egressGate := newEgressGate()
	var (
		setupWG  sync.WaitGroup
		proxyCmd *exec.Cmd
	)
	// Routes sbxproxy audit events to each packed sandbox's own broker by source IP.
	packRouter := newProxyRouter()
	// fuseCtl is the pod's single shared sbxfuse process (design §9): every
	// sandbox's workspaces are served from it, added/removed over its stdin command
	// channel. It runs on fsCtx so it outlives the start of shutdown (snapshot
	// capture reads through its mounts) and is torn down by cancelFS. Each packed
	// sandbox's mountManager drives this one process.
	fuseCtl, err := startFuseControl(fsCtx, &children, fuseBin)
	if err != nil {
		log.Fatalf("start sbxfuse control: %v", err)
	}

	// 1. sbxproxy in transparent mode. iptables (set up below) will REDIRECT all
	// outbound TCP from agent processes here; sbxproxy recovers the original
	// destination via SO_ORIGINAL_DST and dispatches by protocol sniff. The agent
	// itself is unaware of the proxy — no HTTP_PROXY env, no opt-in required.
	setupWG.Add(1)
	go func() {
		defer setupWG.Done()
		// The pod has no boot workload; each packed sandbox writes its own egress
		// rules on create. Seed an empty allowlist so sbxproxy starts.
		if err := writeJSON(rulesTmp, []proxy.EgressRule{}); err != nil {
			log.Fatalf("write rules: %v", err)
		}
		// Generate the per-pod CA (written to caCertPath/caKeyPath). sbxproxy
		// uses it to mint leaf certs; the backend later installs the cert PEM
		// into the workload trust store via InstallCA.
		sandboxd.GenerateCaCert(caCertPath, caKeyPath)
		proxyArgs := []string{
			"-transparent",
			"-addr", proxyAddr,
			"-dns-addr", dnsAddr,
			"-rules", rulesTmp,
			"-mark", fmt.Sprintf("%d", soMark),
			"-ca-cert", caCertPath,
			"-ca-key", caKeyPath,
		}
		// Upstream connection-pool scope. Default "vm" (per-source isolation); a pool
		// (e.g. browser) sets HIVER_PROXY_UPSTREAM_POOL_SCOPE=pod to share warm
		// upstream connections across all sandboxes in the pod — a fresh sandbox's
		// first request then reuses a sibling's warm connection (faster goto), at the
		// cost of per-VM connection isolation (see proxy.upstreamPool).
		if scope := os.Getenv("HIVER_PROXY_UPSTREAM_POOL_SCOPE"); scope != "" {
			proxyArgs = append(proxyArgs, "-upstream-pool-scope", scope)
		}
		// Route proxy audit events to each packed sandbox's own broker by source
		// IP (each packed sandbox has a distinct source IP).
		proxyHandler := packRouter.handle
		// sbxproxy multiplexes control records (egress-reload acks) onto the same
		// events fd as audit events. Peel those off before the audit translator so
		// they advance the egress gate instead of being mis-rendered as agent ops.
		auditOnEvent := sidecarOnEvent(formatProxyEvent, proxyHandler)
		onProxyEvent := func(ev map[string]any) {
			if t, _ := ev["type"].(string); t == "control" {
				if c, _ := ev["control"].(string); c == "egress_reload" {
					egressGate.markApplied(uint64(intField(ev, "generation")))
				}
				return
			}
			auditOnEvent(ev)
		}
		cmd, err := startSidecar(ctx, &children, "sbxproxy", proxyBin, proxyArgs, nil, onProxyEvent)
		if err != nil {
			log.Fatalf("start proxy: %v", err)
		}
		if err := waitForListen(ctx, proxyAddr, 5*time.Second); err != nil {
			_ = cmd.Process.Kill()
			log.Fatalf("proxy did not become ready: %v", err)
		}
		// No pod-level iptables OUTPUT REDIRECT: there is no boot workload sharing
		// the pod netns to confine. Each packed sandbox installs its own per-veth
		// PREROUTING/FORWARD rules instead (see container_net_linux).
		proxyCmd = cmd
	}()

	// 2. sbxfuse is the pod's single shared daemon (started above as fuseCtl);
	// each packed sandbox adds/removes its own workspaces over its command channel.
	// There are no pod-level workspaces to reconcile here.
	setupWG.Wait()
	phase.mark("proxy + fuse startup")

	// This pod hosts same-image sandboxes created on demand via POST /v1/<key>.
	// There is no boot sandbox — extract the shared image config + CA, hand the
	// shared sidecars to the supervisor, and park. Each POST then packs a sandbox
	// (own netns/IP, overlay, cgroup, per-source egress) via createPacked.
	//
	// In pack mode (--pack) the pod persists until SIGTERM and serves N sandboxes
	// (no pod-level TTL; each packed sandbox has its own). Without --pack it hosts a
	// single sandbox: createPacked's teardown calls shutdown when that sandbox is
	// gone, so the process exits with it (see packState.single).
	imgCfg, err := runc.ExtractImageConfig()
	if err != nil {
		log.Fatalf("unpack sandbox config: %v", err)
	}
	caData, _ := os.ReadFile(caCertPath)
	sup.mu.Lock()
	sup.pack = &packState{
		ctx:    ctx,
		single: !*pack,
		// In single-sandbox mode the teardown calls this once the sole sandbox is
		// gone. It must cancel BOTH contexts: ctx stops sbxproxy, fsCtx stops the
		// shared sbxfuse — children.Wait() below blocks on both, so cancelling only
		// ctx would leave sbxfuse running and the process would hang instead of
		// exiting. Safe here because the teardown has already captured the snapshot
		// and stopped this sandbox's mounts before invoking shutdown.
		shutdown:    func() { cancel(); cancelFS() },
		children:    &children,
		isoKind:     isoKind,
		hostname:    podHostname,
		soMark:      soMark,
		proxyPort:   proxyPort,
		dnsPort:     dnsPort,
		proxyPID:    proxyCmd.Process.Pid,
		rulesPath:   rulesTmp,
		caData:      caData,
		imgCfg:      imgCfg,
		fuse:        fuseCtl,
		workDir:     workDir,
		snapshotDir: *snapshotDir,
		router:      packRouter,
		egressGate:  egressGate,
	}
	if *maxConcurrentLaunches > 0 {
		sup.pack.launchSem = make(chan struct{}, *maxConcurrentLaunches)
		log.Printf("sandboxd: capping concurrent launches at %d", *maxConcurrentLaunches)
	}
	// Preallocate a warm pool of sandbox network slots (netns/veth/iptables + DNS
	// sink) so claims skip that contended setup on the request path. Off the
	// request path and serialized on one worker, so the xtables-lock contention a
	// concurrent create burst otherwise pays is gone.
	if *preallocPool > 0 {
		sup.pack.pool = newPreallocPool(sup.pack, *preallocPool)
		sup.pack.pool.start()
		log.Printf("sandboxd: preallocating %d sandbox slots", *preallocPool)
	}
	sup.mu.Unlock()
	// Drain coalesced egress reloads for the pod's lifetime.
	go sup.pack.runEgressReloader(ctx)
	sup.bootComplete()
	phase.mark("pack pod host ready")
	if *pack {
		log.Println("sandboxd: pack host ready; POST /v1/<key> to pack sandboxes")
	} else {
		log.Println("sandboxd: single-sandbox host ready; POST /v1/<key> to start the sandbox (process exits when it's gone)")
	}
	// Nothing is snapshotted at pod start: each packed sandbox cold-boots, or
	// resumes a per-sandbox VM snapshot the client captured earlier under its
	// snapshot.vm.key (vmStateDir in createPacked).
	<-ctx.Done()
	children.Wait()
}

// injectGatewayURL forwards the pod's gateway base URL into the workload
// environment so an agent inside the sandbox can reach the gateway (e.g. to
// create nested sandboxes). The value is set on the pod by the runtime — docker
// compose / the helm chart — and read here from sandboxd's own environment.
// A user-supplied value in the spec wins; an unset/empty pod value is a no-op.
func injectGatewayURL(env map[string]string) {
	if _, ok := env[spec.EnvGatewayURL]; ok {
		return
	}
	if v := os.Getenv(spec.EnvGatewayURL); v != "" {
		env[spec.EnvGatewayURL] = v
	}
}

// startChild spawns name with the given args/env, forwards its stdout/stderr
// to ours (verbatim, see streamLines), and tracks completion via wg.
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
func startChild(ctx context.Context,
	wg *sync.WaitGroup,
	name, bin string,
	args, env []string,
	extraFiles []*os.File,
	onStdio func(stream, line string)) (cmd *exec.Cmd, stdioDone <-chan struct{}, err error) {
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
	done, err := superviseStdio(wg, name, cmd, onStdio)
	if err != nil {
		return nil, nil, err
	}
	return cmd, done, nil
}

// superviseStdio attaches line streaming (and optional broker publishing via
// onStdio) to an already-built command, starts it, and returns a channel that
// closes once both stdio pipes have hit EOF — the stdioDone contract startChild
// documents. It is the shared core of startChild and is also used directly by
// callers that build their own *exec.Cmd, e.g. the snapshot-resume path, whose
// "agent" is an exec session (iso.ExecCmd) rather than a freshly spawned binary.
func superviseStdio(wg *sync.WaitGroup, name string, cmd *exec.Cmd, onStdio func(stream, line string)) (<-chan struct{}, error) {
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
		streamLines(name+":out", stdout, outCb)
	}()
	go func() {
		defer wg.Done()
		defer stdioWg.Done()
		streamLines(name+":err", stderr, errCb)
	}()
	go func() {
		stdioWg.Wait()
		close(done)
	}()
	return done, nil
}

// startAgentTTY launches the agent attached to a pseudo-terminal and returns
// the command (for the caller to Wait on) and the pty Session clients attach
// to via /v1/exec-stream. It mirrors startChild's process supervision (SIGTERM
// on ctx cancel, then a kill grace via WaitDelay) but, because a tty merges
// stdout and stderr into one stream, fans the terminal bytes out through the
// Session instead of the two line-streamed pipes.
//
// Unlike the pipes path, the entrypoint's tty output is neither mirrored to
// sandboxd's stdout nor published as StdioEvents: a terminal stream is raw
// bytes (cursor moves, redraws, colour escapes), not line-oriented log output,
// so logging it would spam the container logs and surfacing it in the event
// feed would just be noise. Clients consume it by attaching to the Session
// (this matches the interactive `exec-stream --tty` path, which also skips
// stdio events for the same reason).
//
// The Session's Done channel stands in for startChild's stdioDone: it closes
// once the master reaches EOF (the agent's output is finished).
// entrypointIsTail reports whether the effective container entrypoint is the
// `tail` keepalive pattern, either bare (ENTRYPOINT/CMD starting with tail) or
// wrapped in a shell that execs it. When true there is no interactive process to
// attach a pty to.
func entrypointIsTail(imgCfg *runc.ImageConfig) bool {
	argv := imgCfg.Entrypoint
	if len(argv) == 0 {
		argv = imgCfg.Cmd
	}
	if len(argv) == 0 {
		return false
	}
	first := argv[0]
	if i := strings.LastIndex(first, "/"); i >= 0 {
		first = first[i+1:]
	}
	if first == "tail" {
		return true
	}
	// A keepalive can also be wrapped in a shell that runs some setup and then
	// execs the tail keepalive — e.g. the claude image's prewarm entrypoint warms
	// its binary and signals readiness before `exec tail -f /dev/null`. Recognize
	// that shape too so the tty-attach drop still fires: there is still no
	// interactive process to attach a pty to.
	switch first {
	case "sh", "bash", "dash", "ash":
		if len(argv) >= 3 && argv[1] == "-c" {
			return strings.HasSuffix(strings.TrimSpace(argv[len(argv)-1]), "tail -f /dev/null")
		}
	}
	return false
}

func startAgentTTY(ctx context.Context, bin string, args []string) (*exec.Cmd, *pty.Session, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	master, err := pty.Start(cmd)
	if err != nil {
		return nil, nil, err
	}
	return cmd, pty.NewSession(master, nil), nil
}

// streamLines forwards a child's stdio to sandboxd's stdout verbatim — no
// per-line prefix. The children (sbxproxy, sbxfuse, the agent) already
// self-identify in their own log lines, so a "[name:err]" wrapper just added
// noise and made routine sidecar logs (which a child writes to its stderr by
// convention) look like errors. tag names the stream only for the rare
// read-error diagnostic below.
func streamLines(tag string, r io.Reader, onLine func(string)) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			fmt.Fprint(os.Stdout, line)
			if onLine != nil {
				onLine(line)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "[%s] read error: %v\n", tag, err)
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
		// Tight poll: the FUSE mount lands within a few ms of the control-channel
		// mount command, but this gates the create critical path — a 100ms tick made
		// every workspace mount cost ~100ms (one full sleep) even though the mount was
		// ready almost immediately. Statfs is a cheap syscall, so poll fast.
		time.Sleep(5 * time.Millisecond)
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
// writeJSON atomically writes v as JSON to path: it encodes into a temp file in
// the same directory and renames it over the target. The rename is atomic on the
// same filesystem, so a concurrent reader (e.g. sbxproxy re-reading the egress
// rules on SIGHUP) always sees either the complete old file or the complete new
// one — never a half-written one. A plain os.Create truncates in place, so under
// a burst of rewrites (high-qps egress reloads) the reader can catch a torn file
// and fail the parse ("unexpected end of JSON input"), dropping the new rules.
func writeJSON(path string, v any) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds
	if err := json.NewEncoder(tmp).Encode(v); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// localFSMounts returns a MountSource for every local-backend FS entry.
// Local backends store their data in the -backend directory (not the overlayfs
// upper layer), so snapshot capture and restore must route those paths there.
//
// key namespaces the host backend dir for a packed sandbox: its workspaces live
// under /run/sandboxd/<key>/backend/<slug>, mirroring mountManager.hostBackend,
// not the boot sandbox's host==guest <mount>-backend layout. Getting this wrong
// makes snapshot capture walk an empty path and silently drop the workspace.
func localFSMounts(key string, fsList []spec.FS) []snapshot.MountSource {
	var mounts []snapshot.MountSource
	for i := range fsList {
		f := &fsList[i]
		if f.Backend == spec.BackendLocal {
			hostDir := f.BackendPath()
			if key != "" {
				hostDir = filepath.Join("/run/sandboxd", key, "backend", f.Slug())
			}
			mounts = append(mounts, snapshot.MountSource{
				ContainerPath: f.Mount,
				HostDir:       hostDir,
			})
		}
	}
	return mounts
}

// intOrZero dereferences an optional spec int, returning 0 when unset so
// isolation.New applies its default.
func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ceilVcpu converts the optional fractional cpu (the limit) into a whole guest
// vCPU count, rounding up so the guest never gets fewer cores than requested.
// Returns 0 when unset or non-positive so isolation.New applies its default.
func ceilVcpu(p *float64) int {
	if p == nil || *p <= 0 {
		return 0
	}
	return int(math.Ceil(*p))
}

// vmStateDir returns this sandbox's VM state directory under -snapshot-dir
// (/snapshots/vm-<key>, keyed by snapshot.vm.key) — the source of truth where its
// overlay/metadata/snapshot live, used for both cold boot (created there) and
// resume (reopened there). Empty when the config names no vm key or no local
// snapshot dir is configured. Whether the dir already holds a resumable snapshot
// is the isolation backend's call (isolation.VMSnapshotReady), not this helper's.
// vmStateDir returns this microvm's state directory under snapshotDir and whether
// it is ephemeral. A client-chosen snapshot.vm.key yields a stable, persistent dir
// (the source of truth, resumed across get-or-create). Without a key, a random key
// gives the VM a private dir so its overlay can still be captured — and a later
// snapshot relocated to a named key (see microvm.SnapshotLive) — but, unrequested,
// it is hard to reuse and torn down with the VM (Config.VMStateEphemeral). Empty
// snapshotDir → no state dir (overlay stays ephemeral in the jail, no VM snapshots).
func vmStateDir(snapshotDir string, sp *spec.Spec) (dir string, ephemeral bool) {
	if snapshotDir == "" {
		return "", false
	}
	if sp.Snapshot != nil && sp.Snapshot.VM != nil && sp.Snapshot.VM.Key != "" {
		return snapshot.VMSnapshotDir(snapshotDir, sp.Snapshot.VM.Key), false
	}
	return snapshot.VMSnapshotDir(snapshotDir, "ephemeral-"+randHex(16)), true
}

// randHex returns a hex string of n random bytes, used to mint a collision-
// resistant, hard-to-guess key for an ephemeral VM state dir.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is fatal-grade, but a snapshot dir name need not be:
		// fall back to a timestamp so boot still proceeds (uniqueness, not secrecy,
		// is what matters for a per-pod ephemeral dir).
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// isolationLocalMounts converts the local-backend FS list into the
// backend-agnostic form the isolation backend snapshots against. key is the
// packed-sandbox key ("" for the boot sandbox) used to locate the per-key host
// backend dirs.
func isolationLocalMounts(key string, fsList []spec.FS) []isolation.SnapshotMount {
	var out []isolation.SnapshotMount
	for _, m := range localFSMounts(key, fsList) {
		out = append(out, isolation.SnapshotMount{ContainerPath: m.ContainerPath, HostDir: m.HostDir})
	}
	return out
}
