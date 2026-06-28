// Command sandboxd is the runtime agent that wires together the
// MITM proxy, FUSE daemon, and agent workload as a single sandbox "pod".
//
// sandboxd is configured by a single JSON spec (see internal/spec), delivered in
// the HIVE_SPEC environment variable by the runtime — or, in pack mode,
// supplied per sandbox via the create/config API. The spec carries everything sandboxd needs:
// the agent binary + args, the workspace's host-side backend and FUSE mount
// point, the FUSE ACLs, the proxy's egress allowlist, and where to write audit
// logs.
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
	"maps"
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
	"github.com/hiver-sh/hiver/internal/api/handlers"
	"github.com/hiver-sh/hiver/internal/events"
	"github.com/hiver-sh/hiver/internal/isolation"
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
		snapshotDir           = flag.String("snapshot-dir", "", "directory where files and VM-state snapshots are stored on local disk (skip the pod overlay — point it at NVMe); optional — when unset, files snapshots only work for configs that route them to a FUSE drive via snapshot.files.mount, and VM snapshots are disabled")
		pack                  = flag.Bool("pack", false, "boot in pack mode: bring up the shared sidecars and park as a pod host; each POST /v1/<key> packs a new same-image sandbox (own netns/IP, overlay, egress) into this pod")
		preallocPool          = flag.Int("prealloc-pool", 10, "pack mode: number of sandbox network slots (netns/veth/iptables + DNS sink) to preallocate and keep warm so a create claims one instead of wiring it on the request path; 0 disables")
		maxConcurrentLaunches = flag.Int("max-concurrent-launches", 10, "pack mode: cap on concurrent sandbox creates in flight, so a burst doesn't oversubscribe the node's cores during the CPU-bound resume/convergence phases; set near the node's vCPU count; 0 disables (unbounded)")
	)
	flag.Parse()

	// The spec is delivered as JSON in the HIVE_SPEC env var (injected by the
	// runtime), not a mounted file. It is optional: in pack mode the pod has no
	// boot workload and the image is whatever is bundled in the pod, so HIVE_SPEC
	// is redundant there — each sandbox's full config arrives later over the API.
	// Guest vCPU/RAM sizing comes from the spec's cpu/memory (the same fields the
	// container backend uses); firecracker bakes those into a VM snapshot.
	sp, err := spec.LoadEnv()
	if err != nil {
		log.Fatalf("spec: %v", err)
	}

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
	isoKind, err := isolation.Detect()
	if err != nil {
		log.Fatalf("isolation: %v", err)
	}
	isoCfg := isolation.Config{
		Hostname:    podHostname,
		LocalMounts: isolationLocalMounts(sp.FS),
		VcpuCount:   ceilVcpu(sp.CPU),
		MemoryMiB:   intOrZero(sp.Memory),
	}
	// VM state dir: when the config names a vm snapshot key, this VM's overlay/
	// metadata/snapshot live under -snapshot-dir (the source of truth), keyed by it.
	// If that dir already holds a snapshot the VM resumes from it in place; otherwise
	// it cold-boots into it and the client captures one via the snapshot action,
	// which a later get-or-create then resumes. Microvm only; the container backend
	// ignores it. Empty ⇒ ephemeral overlay in the jail, no VM snapshots.
	// Without a client key the dir is an auto-assigned ephemeral home (random key),
	// torn down with the VM; it still lets the VM be snapshotted (and that snapshot
	// relocated to a named key). Microvm only; the container backend ignores both.
	if isoKind == isolation.KindMicroVM {
		isoCfg.VMStateDir, isoCfg.VMStateEphemeral = vmStateDir(*snapshotDir, sp)
	}
	if isoCfg.VMStateDir != "" && !isoCfg.VMStateEphemeral && isolation.VMSnapshotReady(isoCfg.VMStateDir) {
		log.Printf("sandboxd: vm snapshot %q found; resuming", sp.Snapshot.VM.Key)
	}
	iso, err := isolation.New(isoKind, isoCfg)
	if err != nil {
		log.Fatalf("isolation: %v", err)
	}
	log.Printf("sandboxd: isolation = %s", iso.Kind())
	phase.mark("isolation init")

	// Seed the in-memory config from the boot spec so GET/PUT /v1/config have
	// something to read/diff against from the first request. The store holds a
	// gen.SandboxConfig; the spec's JSON shape matches, so we round-trip through
	// the generated type for type safety.
	initialCfg, err := configFromSpec(sp)
	if err != nil {
		log.Fatalf("initial config: %v", err)
	}

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

	// The broker is the single fan-out point for the SSE `/v1/events`
	// stream. Sidecar audit events arrive over the per-child socketpair
	// (see startSidecar), get translated to SandboxEvent shape, and
	// Publish'd here; api.NewSandboxServer hands subscribers to the SSE handler.
	broker := events.New(events.DefaultCapacity, 0)
	store := api.NewConfigStore(initialCfg)

	// The boot sandbox is created and launched immediately below from the env
	// (HIVE_SPEC) config, resuming a keyed VM snapshot when one exists on disk.
	// (Pack pods take the earlier return and create sandboxes per POST /v1/<key>.)
	// sb is assigned by claimSandbox (defined once lifetime exists, below); the
	// launch/resume closures and shutdown read it after that.
	var sb *handlers.Sandbox

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
	// Any broker event (except resource.usage, which uses PublishSilent)
	// counts as sandbox activity and resets the inactivity timer.
	broker.SetActivityHook(lifetime.Reset)

	// claimSandbox builds the boot sandbox under key, wires its subsystems, and
	// registers it so keyed routes resolve. cancel (the lifecycle ctx's cancel)
	// backs DELETE /v1/<key>. The TTL countdown only starts once the workload
	// launches (lifetime.Run, below), not here.
	claimSandbox := func(key string) {
		sb = handlers.NewSandbox(key, soMark)
		sb.SetIsolation(iso)
		sb.SetSnapshotDir(*snapshotDir)
		sb.SetBroker(broker)
		sb.SetStore(store)
		sb.SetLifetime(lifetime)
		// Tie exec sessions to the lifecycle ctx so DELETE/shutdown kills them.
		sb.SetLifecycleContext(ctx)
		sup.register(sb, specImage(sp), cancel)
	}

	proxyPort, err := freePort()
	if err != nil {
		log.Fatalf("free port: %v", err)
	}
	// In pack mode each sandbox reaches the proxy/sink via its veth gateway IP
	// (the host DNATs to it), so they must bind all interfaces. The single-sandbox
	// path keeps loopback (its workload shares the pod netns and is REDIRECT'd to
	// 127.0.0.1).
	bindHost := "127.0.0.1"
	if *pack {
		bindHost = "0.0.0.0"
	}
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
		setupWG    sync.WaitGroup
		isoMu      sync.Mutex
		proxyCmd   *exec.Cmd
		packRouter *proxyRouter // non-nil in pack mode; routes proxy events to per-sandbox brokers
	)
	if *pack {
		packRouter = newProxyRouter()
	}
	// fuseCtl is the pod's single shared sbxfuse process (design §9): every
	// sandbox's workspaces are served from it, added/removed over its stdin command
	// channel. It runs on fsCtx so it outlives the start of shutdown (snapshot
	// capture reads through its mounts) and is torn down by cancelFS. Both the boot
	// mountManager and each packed sandbox's mountManager drive this one process.
	fuseCtl, err := startFuseControl(fsCtx, &children, fuseBin)
	if err != nil {
		log.Fatalf("start sbxfuse control: %v", err)
	}

	// mountMgr owns the sbxfuse workspaces: it brings them up at boot and
	// reconciles them (add/remove/re-ACL) when a later config is applied. It runs
	// on fsCtx (not the lifecycle ctx) so the FUSE mounts outlive the start of
	// shutdown — finalizeShutdown captures the snapshot through them, then tears
	// them down via cancelFS.
	mountMgr := newMountManager(fsCtx, &children, broker, iso, &isoMu, fuseCtl, workDir, *snapshotDir, soMark)

	// 1. sbxproxy in transparent mode. iptables (set up below) will REDIRECT all
	// outbound TCP from agent processes here; sbxproxy recovers the original
	// destination via SO_ORIGINAL_DST and dispatches by protocol sniff. The agent
	// itself is unaware of the proxy — no HTTP_PROXY env, no opt-in required.
	setupWG.Add(1)
	go func() {
		defer setupWG.Done()
		if err := writeJSON(rulesTmp, sp.Egress); err != nil {
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
		// In pack mode, route proxy audit events to per-sandbox brokers by
		// source IP (each packed sandbox has a distinct source IP). In
		// single-sandbox mode all events go to the one shared broker.
		var proxyHandler func(map[string]any)
		if packRouter != nil {
			proxyHandler = packRouter.handle
		} else {
			proxyHandler = newProxyTranslator(broker).handle
		}
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
		// 1b. Install iptables OUTPUT REDIRECT rules so the agent's outbound TCP
		// (in the shared netns) lands on sbxproxy. The mark rule MUST come first
		// so the proxy's own upstream traffic isn't looped back; the loopback
		// rule keeps in-pod localhost traffic (e.g. unit tests, future control
		// sockets) untouched.
		//
		// Skipped in pack mode: there is no boot workload sharing the pod netns to
		// confine, and these OUTPUT rules — notably the `! -p tcp -j DROP` that
		// blocks the workload's non-TCP egress — would drop the DNS sinks' own UDP
		// replies (which originate in the pod netns). Each packed sandbox installs
		// its own per-veth PREROUTING/FORWARD rules instead (see container_net_linux).
		if !*pack {
			isoMu.Lock()
			err = iso.RedirectEgress(ctx, proxyPort, dnsPort, soMark)
			isoMu.Unlock()
			if err != nil {
				log.Fatalf("install egress redirect: %v", err)
			}
			log.Printf("sandboxd: iptables OUTPUT nat redirect → %s installed (mark=0x%x)", proxyAddr, soMark)
		}
		proxyCmd = cmd
	}()

	// 2. sbxfuse — one daemon per fs entry, always on the host. Each gets its
	// own ACL file, audit log, mount point, and backend dir; remote backends
	// also get their own oplog + uploader inside their own sbxfuse process so
	// a stuck remote on one mount can't block writes to another.
	//
	// The backend then exposes the host mount to the workload: a no-op bind
	// for the container (shared mount namespace), or a 9p-over-vsock export
	// for the microvm (the guest mounts it). Either way sbxfuse — with its
	// ACLs, audit events, and remote backends — stays host-side. Remote
	// backends do their bootstrap fetch unredirected if the egress goroutine
	// hasn't installed iptables yet, which is harmless: their sockets carry
	// soMark and would bypass the REDIRECT anyway.
	setupWG.Add(1)
	go func() {
		defer setupWG.Done()
		// Boot is just an initial reconcile from an empty set — the same mount
		// manager path a later config-apply uses, so the two can't drift. This
		// runs before MountRoot (so it seeds, doesn't restore); a snapshot is
		// restored by the post-MountRoot reconcile below.
		if err := mountMgr.Reconcile(sp); err != nil {
			log.Fatalf("mount workspaces: %v", err)
		}
	}()

	setupWG.Wait()
	phase.mark("proxy + fuse startup")

	// Reconcile sidecar policy whenever the API publishes a config.apply event
	// (a runtime PUT /v1/<key>/config on a running sandbox): re-derive egress
	// rules + per-mount ACLs from the persisted config, rewrite the files each
	// sidecar reads, and reload. Sidecars keep the current policy on read errors
	// so a half-written file can't relax access by accident. The prewarm launch
	// is no longer triggered by the first config (it is the POST /v1/<key> claim
	// below), so no firstConfig latch is passed.
	go reconcileSidecars(ctx, broker, proxyCmd.Process.Pid, store, rulesTmp, mountMgr, nil)

	// prepareWorkload runs the config-independent half of the bring-up: unpack
	// the image config, seed the workspaces from the image, assemble the root
	// filesystem, and install the sandbox CA. None of it depends on the applied
	// config, so in prewarm mode it runs at boot — before waiting for the first
	// PUT /v1/config — leaving only launchWorkload to pay for on claim. Returns
	// the base image config for launchWorkload to apply overrides to.
	prepareWorkload := func() *runc.ImageConfig {
		// 3. Agent. Egress + workspace are now mediated.

		imgCfg, err := runc.ExtractImageConfig()
		if err != nil {
			log.Fatalf("unpack sandbox config: %v", err)
		}
		phase.mark("image unpack")

		// Assemble the agent root filesystem (overlay/drives). Must come after the
		// workspaces are reconciled (the mount manager seeds them, moving image
		// files out of these paths so the overlay lower reflects clean content).
		if err := iso.MountRoot(); err != nil {
			log.Fatalf("mount agent root filesystem: %v", err)
		}
		// The overlay is assembled, so the post-MountRoot reconcile may now restore
		// a snapshot into it.
		mountMgr.SetRootMounted()
		phase.mark("seed + root filesystem assembly")

		// Install the sandbox CA into the workload trust store so sbxproxy can
		// terminate TLS. The backend places it where the workload will see it
		// (merged rootfs for container; the guest's trust store for microvm).
		if caData, err := os.ReadFile(caCertPath); err == nil {
			if err := iso.InstallCA(caData); err != nil {
				log.Printf("sandboxd: install sandbox CA: %v", err)
			}
		}
		return imgCfg
	}

	// launchWorkload runs the config-dependent half: apply the entrypoint/cwd
	// overrides, then start the agent (runc/firecracker) and wire the readiness +
	// graceful-shutdown goroutines. The snapshot has already been restored by the
	// reconcile that precedes this call. It returns once the agent is running;
	// main then waits on ctx below as before.
	launchWorkload := func(sp *spec.Spec, imgCfg *runc.ImageConfig) {
		if len(sp.Entrypoint) > 0 {
			imgCfg.Entrypoint = sp.Entrypoint
			imgCfg.Cmd = nil
		}
		if sp.Cwd != "" {
			imgCfg.WorkingDir = sp.Cwd
		}

		agentEnv := make(map[string]string, len(sp.Env)+1)
		maps.Copy(agentEnv, sp.Env)
		agentEnv["NODE_EXTRA_CA_CERTS"] = isolation.NodeCACertPath

		mounts := make([]runc.BindMount, 0, len(sp.FS)+2)
		for i := range sp.FS {
			// Internal mounts stay host-side only (e.g. a remote-backed snapshot
			// target sandboxd reads/writes for capture/restore): the agent must
			// never see them, so they are not bind-mounted into the container. For
			// the container backend this bundle list is the actual export path
			// (ExportWorkspace is a no-op), so skipping here is what hides them.
			if sp.FS[i].Internal {
				continue
			}
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

		// tty: true wraps the entrypoint in a pseudo-terminal so clients can attach
		// to it via /v1/exec-stream. Only the container backend supports it (the
		// microvm runs the workload in a guest the host pty can't reach), so ignore
		// it elsewhere rather than wrapping the wrong process.
		ttyEnabled := sp.Tty != nil && *sp.Tty
		if ttyEnabled && iso.Kind() != isolation.KindContainer {
			log.Printf("sandboxd: tty: ignoring tty option (only supported for %q isolation, got %q)", isolation.KindContainer, iso.Kind())
			ttyEnabled = false
		}
		if ttyEnabled {
			// The entrypoint runs on a real terminal, so advertise one: without
			// TERM, programs assume a dumb terminal and disable colour and cursor
			// control. The interactive-exec path sets these per-session, but an
			// attach inherits the entrypoint's own environment, so they must be set
			// here at launch. User-supplied env wins.
			if _, ok := agentEnv["TERM"]; !ok {
				agentEnv["TERM"] = "xterm-256color"
			}
			if _, ok := agentEnv["COLORTERM"]; !ok {
				agentEnv["COLORTERM"] = "truecolor"
			}
		}

		// A keepalive-only entrypoint (e.g. tail -f /dev/null) has no interactive
		// output to attach to; skip the pty so entrypointTTY stays nil.
		if ttyEnabled && entrypointIsTail(imgCfg) {
			ttyEnabled = false
		}
		// Hand the agent workload off to the isolation backend: it writes any
		// runtime config it needs (the OCI bundle for runc) and returns the
		// command that launches the workload, which we run through our own
		// child supervisor for stdio streaming + lifecycle tracking.
		agentBin, agentArgs, err := iso.LaunchAgent(isolation.AgentConfig{
			ImageConfig: imgCfg,
			Env:         agentEnv,
			Mounts:      mounts,
			Hostname:    podHostname,
			TTY:         ttyEnabled,
		})
		if err != nil {
			log.Fatalf("prepare agent: %v", err)
		}
		// The agent runs on its own context so that on shutdown we can flush the
		// workload filesystem (sync the microvm guest) *before* the workload is
		// stopped — otherwise the guest's recent writes never reach the overlay
		// image and the captured snapshot is stale. The container backend's flush
		// is a no-op.
		agentCtx, stopAgent := context.WithCancel(context.Background())
		// No defer here: launchWorkload returns while the agent keeps running. The
		// flush goroutine below calls stopAgent on ctx cancel (signal, TTL, or the
		// agent's own exit), which is the single stop path.

		// agentStdioDone closes once the agent's output is fully drained (both
		// stdio pipes EOF, or the tty master EOFs). entrypointTTY is non-nil only
		// on the tty path; it backs exec-stream attach requests.
		var (
			agentCmd       *exec.Cmd
			agentStdioDone <-chan struct{}
			entrypointTTY  *pty.Session
		)
		if ttyEnabled {
			cmd, sess, ttyErr := startAgentTTY(agentCtx, agentBin, agentArgs)
			if ttyErr != nil {
				log.Fatalf("start agent (tty): %v", ttyErr)
			}
			agentCmd, entrypointTTY, agentStdioDone = cmd, sess, sess.Done()
			log.Printf("sandboxd: entrypoint attached to tty")
		} else {
			cmd, done, startErr := startChild(agentCtx, &children, "sandbox", agentBin,
				agentArgs, nil, nil,
				publishAgentStdio(broker))
			if startErr != nil {
				log.Fatalf("start agent: %v", startErr)
			}
			agentCmd, agentStdioDone = cmd, done
		}
		// The API server is already serving (started above); publish the entrypoint
		// pty now that it exists so exec-stream attach requests can reach it.
		if entrypointTTY != nil {
			sb.SetEntrypointTTY(entrypointTTY)
		}
		// The workload is now committed with its boot-time config, so freeze those
		// fields against further ApplyConfig changes (cpu/memory/entrypoint/cwd/tty/
		// env become no-ops from here). In the prewarm flow this is the point the
		// sandbox transitions from configurable to started.
		sb.SetStarted()
		// The workload is running, so a later config-apply that adds a workspace
		// must inject it into the live workload rather than rely on launch.
		mountMgr.SetWorkloadLive()
		phase.mark("agent launch")

		// Wait for the inner sandbox to come up, then notify the API — this flips it
		// out of its "still starting" state (500, or 503 on /v1/ping) into serving
		// real requests. WaitReady polls the runtime; doing it once here, off the
		// request path, and broadcasting beats re-probing on every keepalive ping.
		go func() {
			if err := iso.WaitReady(ctx); err != nil {
				log.Printf("sandboxd: wait for sandbox ready: %v", err)
				return
			}
			phase.mark("sandbox ready")
			sb.NotifyReady()
			// Stream the guest workload's stdout/stderr as stdio events (microvm: the
			// workload runs in the guest, so its output isn't on the firecracker child's
			// stdio; container: no-op). Makes a failing workload visible in the inspector.
			go iso.StreamWorkloadLogs(ctx, publishAgentStdio(broker))
		}()

		children.Go(func() {
			<-ctx.Done()
			flushCtx, cancelFlush := context.WithTimeout(context.Background(), fsDrainTimeout)
			if err := iso.FlushAgent(flushCtx); err != nil {
				log.Printf("sandboxd: flush agent before stop: %v", err)
			}
			cancelFlush()
			stopAgent()
		})

		go api.PollResourceUsage(ctx, broker, iso.CgroupPath())

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
		children.Go(func() {
			<-agentStdioDone
			_ = agentCmd.Wait()
			log.Println("sandboxd: agent finished")
			finalizeShutdown(*snapshotDir, store, iso, broker, s, cancelFS, cancel)
		})
	}

	// resumeWorkload is the VM-resume fast path: the workload is already warm in a
	// VM snapshot (a keyed snapshot the client captured, or a pack base snapshot),
	// so the launch just makes it serving and injects the config's workspaces — it
	// does not cold-launch the entrypoint. The microvm backend starts a fresh VMM
	// and loads the snapshot (in which the entrypoint
	// is already running); the container backend's entrypoint container is already
	// running, so it starts no child. Teardown flushes (microvm) and stops the
	// workload.
	resumeWorkload := func(sp *spec.Spec, imgCfg *runc.ImageConfig) {
		// Start the resume process: a fresh VMM for the microvm (supervised like
		// the cold path); empty for the container, whose entrypoint container is
		// already running — nothing to start.
		vmBin, vmArgs, err := iso.ResumeAgent()
		if err != nil {
			log.Fatalf("prepare resume: %v", err)
		}
		var (
			vmCmd       *exec.Cmd
			vmStdioDone <-chan struct{}
			stopVM      = func() {}
		)
		if vmBin != "" {
			// The VMM runs on its own context so teardown can flush the guest before
			// the VM is stopped (mirrors the cold path's agentCtx). A nil onStdio
			// supervises the firecracker process without publishing its boot noise.
			vmCtx, cancel := context.WithCancel(context.Background())
			stopVM = cancel
			cmd, done, startErr := startChild(vmCtx, &children, "sandbox", vmBin, vmArgs, nil, nil, nil)
			if startErr != nil {
				stopVM()
				log.Fatalf("start resume vm: %v", startErr)
			}
			vmCmd, vmStdioDone = cmd, done
			resumeCtx, cancelResume := context.WithTimeout(ctx, snapshotResumeTimeout)
			err = iso.ResumeReady(resumeCtx)
			cancelResume()
			if err != nil {
				log.Fatalf("resume snapshot: %v", err)
			}
		} else if err := iso.ResumeReady(ctx); err != nil {
			log.Fatalf("resume ready: %v", err)
		}
		phase.mark("resume")

		// Resolve the workload environment from the image config + applied spec and
		// deliver it along with the first config's workspaces into the running
		// workload. The already-running entrypoint won't pick up late env (the
		// browser host doesn't need it), but the microvm guest's process env (for
		// exec sessions) and the workspaces both matter — workspaces can't be baked
		// into a snapshot that predates the config.
		env := make(map[string]string, len(imgCfg.Env)+len(sp.Env)+1)
		for _, kv := range imgCfg.Env {
			if i := strings.IndexByte(kv, '='); i > 0 {
				env[kv[:i]] = kv[i+1:]
			}
		}
		maps.Copy(env, sp.Env)
		env["NODE_EXTRA_CA_CERTS"] = isolation.NodeCACertPath
		// A tty entrypoint override runs on a terminal; advertise one so programs
		// enable colour/cursor control (user-supplied env wins).
		ttyReq := len(sp.Entrypoint) > 0 && sp.Tty != nil && *sp.Tty
		if ttyReq {
			if _, ok := env["TERM"]; !ok {
				env["TERM"] = "xterm-256color"
			}
			if _, ok := env["COLORTERM"]; !ok {
				env["COLORTERM"] = "truecolor"
			}
		}
		envSlice := make([]string, 0, len(env))
		for k, v := range env {
			envSlice = append(envSlice, k+"="+v)
		}
		rs := isolation.ResumeState{Env: envSlice, MountWorkspacesSync: true}
		// The snapshot restores the workload it captured running — the config's own
		// entrypoint, captured at cold-boot. We do NOT relaunch it: a non-tty
		// entrypoint comes back on the guest console (relaunching would duplicate the
		// workload), and changing the entrypoint/cwd of a resumed (already-running)
		// workload is a no-op, exactly like changing it on any started sandbox.
		//
		// A tty entrypoint is the one exception: it ran as a guest tty exec session
		// whose host-side pty bridge does not survive a snapshot/restore, so re-establish
		// it below (setupEntrypointTTY runs the recorded override as a fresh session).
		if ttyReq {
			iso.PrepareEntrypointTTY(sp.Entrypoint, sp.Cwd, envSlice)
		}
		if err := iso.ApplyResumeState(ctx, rs); err != nil {
			log.Printf("sandboxd: apply resume state: %v", err)
		}
		setupEntrypointTTY(ctx, "boot", iso, sb, cancel)

		// The workload is committed; freeze boot-time config fields, mark the
		// workload live, flip the server to started, and announce readiness (the
		// workload is already up).
		sb.SetStarted()
		mountMgr.SetWorkloadLive()
		phase.mark("resume ready")
		sb.NotifyReady()

		go api.PollResourceUsage(ctx, broker, iso.CgroupPath())
		// Stream the guest workload's stdout/stderr as stdio events so a failing
		// workload is visible in the inspector (microvm guest output isn't on the
		// firecracker child's stdio; container backend is a no-op).
		go iso.StreamWorkloadLogs(ctx, publishAgentStdio(broker))

		// Teardown. microvm: on lifecycle cancel flush + stop the VMM, and finalize
		// when the VMM exits — which also covers the guest powering itself off when
		// its workload exits — mirroring the cold path's two goroutines. container:
		// on lifecycle cancel, StopAgent then finalize.
		if vmBin != "" {
			children.Go(func() {
				<-ctx.Done()
				flushCtx, cancelFlush := context.WithTimeout(context.Background(), fsDrainTimeout)
				if err := iso.FlushAgent(flushCtx); err != nil {
					log.Printf("sandboxd: flush agent before stop: %v", err)
				}
				cancelFlush()
				stopVM()
			})
			children.Go(func() {
				<-vmStdioDone
				_ = vmCmd.Wait()
				log.Println("sandboxd: agent finished")
				finalizeShutdown(*snapshotDir, store, iso, broker, s, cancelFS, cancel)
			})
		} else {
			children.Go(func() {
				<-ctx.Done()
				stopCtx, cancelStop := context.WithTimeout(context.Background(), fsDrainTimeout)
				if err := iso.StopAgent(stopCtx); err != nil {
					log.Printf("sandboxd: stop agent: %v", err)
				}
				cancelStop()
				finalizeShutdown(*snapshotDir, store, iso, broker, s, cancelFS, cancel)
			})
		}
	}

	// Pack mode: this pod is a host for N same-image sandboxes created on demand
	// via POST /v1/<key>. There is no boot sandbox — don't MountRoot/launch on the
	// boot iso. Extract the shared image config + CA, hand the shared sidecars to
	// the supervisor, and park. Each POST then packs a sandbox (own netns/IP,
	// overlay, cgroup, per-source egress) via createPacked. The pod persists until
	// SIGTERM (no pod-level TTL; each packed sandbox has its own).
	if *pack {
		imgCfg, err := runc.ExtractImageConfig()
		if err != nil {
			log.Fatalf("unpack sandbox config: %v", err)
		}
		caData, _ := os.ReadFile(caCertPath)
		sup.mu.Lock()
		sup.pack = &packState{
			ctx:         ctx,
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
			log.Printf("sandboxd: pack — capping concurrent launches at %d", *maxConcurrentLaunches)
		}
		// Preallocate a warm pool of sandbox network slots (netns/veth/iptables + DNS
		// sink) so claims skip that contended setup on the request path. Off the
		// request path and serialized on one worker, so the xtables-lock contention a
		// concurrent create burst otherwise pays is gone.
		if *preallocPool > 0 {
			sup.pack.pool = newPreallocPool(sup.pack, *preallocPool)
			sup.pack.pool.start()
			log.Printf("sandboxd: pack — preallocating %d sandbox slots", *preallocPool)
		}
		sup.mu.Unlock()
		// Drain coalesced egress reloads for the pod's lifetime.
		go sup.pack.runEgressReloader(ctx)
		sup.bootComplete()
		phase.mark("pack pod host ready")
		log.Println("sandboxd: pack — pod host ready; POST /v1/<key> to pack sandboxes")
		// Nothing is snapshotted at pod start: each packed sandbox cold-boots, or
		// resumes a per-sandbox VM snapshot the client captured earlier under its
		// snapshot.vm.key (vmStateDir in createPacked).
		<-ctx.Done()
		children.Wait()
		return
	}

	// Prepare the workload eagerly — config-independent, so this runs before the
	// launch/resume decision below.
	imgCfg := prepareWorkload()

	// The boot sandbox is created and launched immediately from the env
	// (HIVE_SPEC) config under HIVE_KEY. When the config named a VM snapshot key
	// whose state dir holds a snapshot, the backend resumes the warm VM instead of
	// cold-booting (see resumeWorkload).
	key := os.Getenv("HIVE_KEY")
	if key == "" {
		key = "default"
	}
	claimSandbox(key)
	sup.bootComplete()
	// Reconcile the workspaces and restore any files snapshot now that the root is
	// mounted — where the boot spec's mounts settle and (on the cold path) its files
	// snapshot restores. maybeRestore skips the files restore when a VM snapshot is
	// being resumed (its overlay already carries the FS state).
	if err := mountMgr.Reconcile(sp); err != nil {
		log.Printf("sandboxd: reconcile workspaces: %v", err)
	}
	// Prefer the VM-resume fast path whenever a VM snapshot exists for this config
	// (its state dir holds one): the snapshot carries the full machine + filesystem,
	// so it supersedes a cold boot + files-snapshot restore. The files
	// snapshot is the cold-boot fallback (no VM snapshot on disk) and stays available
	// as a portable, shutdown-written persistence path.
	if iso.HasPrewarmSnapshot() {
		resumeWorkload(sp, imgCfg)
	} else {
		launchWorkload(sp, imgCfg)
	}

	// The inner workload is now started, so begin the inactivity countdown. Both
	// launch paths return once the workload is running.
	go lifetime.Run(ctx)

	<-ctx.Done()
	children.Wait()
}

// finalizeShutdown runs the post-workload teardown shared by every launch path
// (cold boot and snapshot resume): capture the configured snapshot, tear down
// the FUSE workspace daemons (teardownFS), unmount the overlay, drain SSE
// subscribers, close the broker, shut down the HTTP server, and cancel the
// lifecycle ctx. The caller must have already stopped the workload (and, for the
// microvm, flushed + stopped the guest) so the snapshot capture reads a
// quiescent, durable filesystem.
//
// teardownFS cancels the FUSE sidecars' context — and crucially runs AFTER the
// capture, not before: a snapshot routed through a FUSE drive (snapshot.mount,
// e.g. a remote-backed internal mount) must be written while that daemon is
// still serving, then the cancel lets it drain its upload oplog and unmount.
func finalizeShutdown(snapshotDir string, store *api.ConfigStore, iso isolation.Isolation, broker *events.Broker, s *api.SandboxServer, teardownFS, cancel context.CancelFunc) {
	// Capture snapshot before unmounting. Read the current config from the store
	// so any runtime update to snapshot config is respected.
	if cfg, err := store.Get(); err != nil {
		log.Printf("sandboxd: snapshot: read config: %v", err)
	} else if sn := cfg.Snapshot; sn != nil && sn.Files != nil {
		f := sn.Files
		// Files are captured on shutdown only when explicitly requested via
		// write_on_shutdown; otherwise they are captured only by the snapshot action.
		if f.Key != "" && f.WriteOnShutdown != nil && *f.WriteOnShutdown {
			// snapshot.files.mount routes the tarball to a FUSE drive (still mounted —
			// the FUSE sidecars are torn down by teardownFS below, after this capture);
			// otherwise use the host's local snapshot directory.
			dir := snapshotDir
			if f.Mount != nil && *f.Mount != "" {
				dir = *f.Mount
			}
			if dir != "" {
				var include []string
				if f.Include != nil {
					include = *f.Include
				}
				dst := snapshot.SnapshotPath(dir, f.Key)
				log.Printf("sandboxd: snapshot: capturing %v → %s", include, dst)
				if err := iso.CaptureSnapshot(dst, include); err != nil {
					log.Printf("sandboxd: snapshot capture: %v", err)
				}
			}
		}
	}

	// Snapshot captured: now tear down the FUSE workspace daemons. Cancelling
	// their context lets each drain its remote-upload oplog (so a snapshot just
	// written through a FUSE drive finishes uploading) and unmount. Done here,
	// after the capture, rather than via the lifecycle ctx that already fired at
	// the start of shutdown.
	teardownFS()

	if err := iso.UnmountRoot(); err != nil {
		log.Printf("sandboxd: unmount overlayfs: %v", err)
	}
	if n := broker.SubscriberCount(); n > 0 {
		log.Printf("sandboxd: waiting for %d event subscriber(s) to drain", n)
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), eventDrainTimeout)
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

// reconcileSidecars subscribes to the broker and, for every successful
// config.apply event, rewrites the on-disk policy files each sidecar
// reads (sbxproxy's egress allowlist + each sbxfuse's per-mount ACLs)
// and signals them with SIGHUP. We re-read the full config from disk
// (rather than re-applying just the event's `changes` diff) so a
// missed or out-of-order event can't leave a sidecar stuck on a stale
// policy.
//
// FS changes are delegated to the mount manager, which starts/stops sbxfuse
// daemons for added/removed mounts and rewrites ACLs for the rest.
func reconcileSidecars(ctx context.Context, broker *events.Broker, proxyPID int, store *api.ConfigStore, rulesPath string, mountMgr *mountManager, firstConfig chan struct{}) {
	_, ch, cancel := broker.Subscribe(0)
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
			cfg, err := store.Get()
			if err != nil {
				log.Printf("sandboxd: reconcile: %v", err)
				continue
			}
			desiredSpec, err := specFromConfig(cfg)
			if err != nil {
				log.Printf("sandboxd: reconcile: %v", err)
				continue
			}
			// Rewrite sbxproxy's egress allowlist from the new config and reload it.
			if err := writeJSON(rulesPath, desiredSpec.Egress); err != nil {
				log.Printf("sandboxd: reconcile egress: %v", err)
			} else if err := syscall.Kill(proxyPID, syscall.SIGHUP); err != nil {
				log.Printf("sandboxd: SIGHUP sbxproxy (pid=%d): %v", proxyPID, err)
			}
			if err := mountMgr.Reconcile(desiredSpec); err != nil {
				log.Printf("sandboxd: reconcile fs: %v", err)
			}
			log.Printf("sandboxd: reconciled sidecar policy from config (event id=%d)", entry.ID)
			// Latch the first config: the prewarm path waits on this to launch.
			if firstConfig != nil {
				close(firstConfig)
				firstConfig = nil
			}
		case <-ctx.Done():
			return
		}
	}
}

// localFSMounts returns a MountSource for every local-backend FS entry.
// Local backends store their data in the -backend directory (not the overlayfs
// upper layer), so snapshot capture and restore must route those paths there.
func localFSMounts(fsList []spec.FS) []snapshot.MountSource {
	var mounts []snapshot.MountSource
	for i := range fsList {
		f := &fsList[i]
		if f.Backend == spec.BackendLocal {
			mounts = append(mounts, snapshot.MountSource{
				ContainerPath: f.Mount,
				HostDir:       f.BackendPath(),
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
// backend-agnostic form the isolation backend snapshots against.
func isolationLocalMounts(fsList []spec.FS) []isolation.SnapshotMount {
	var out []isolation.SnapshotMount
	for _, m := range localFSMounts(fsList) {
		out = append(out, isolation.SnapshotMount{ContainerPath: m.ContainerPath, HostDir: m.HostDir})
	}
	return out
}
