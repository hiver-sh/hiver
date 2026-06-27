package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hiver-sh/hiver/internal/api"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
	"github.com/hiver-sh/hiver/internal/events"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/proxy"
	"github.com/hiver-sh/hiver/internal/pty"
	"github.com/hiver-sh/hiver/internal/runc"
	"github.com/hiver-sh/hiver/internal/snapshot"
)

// packState holds the shared, image-level resources a -pack pod uses to bring up
// per-key sandboxes on demand. One pod hosts N sandboxes of the same image, each
// with its own netns/IP, overlay, cgroup, and per-source egress; the sbxproxy
// and the image rootfs are shared. Set by main in pack mode.
type packState struct {
	ctx         context.Context // pod lifecycle (cancelling tears everything down)
	children    *sync.WaitGroup
	isoKind     isolation.Kind // backend for packed sandboxes, detected from the image
	hostname    string
	soMark      int
	proxyPort   int
	dnsPort     int
	proxyPID    int    // SIGHUP'd after the per-source rules file is rewritten
	rulesPath   string // sbxproxy per-source rules file ({srcIP: [rules]})
	caData      []byte
	imgCfg      *runc.ImageConfig
	fuse        *fuseControl // pod-wide shared sbxfuse process
	workDir     string
	snapshotDir string

	router *proxyRouter // routes sbxproxy audit events to per-sandbox broker by src IP

	// egressGate coalesces the per-source rules-file rewrites + SIGHUPs that
	// every create/delete would otherwise issue serially; runEgressReloader
	// drains it. Shared with main's proxy event handler, which advances its
	// applied generation from sbxproxy's echo.
	egressGate *egressGate

	mu     sync.Mutex
	nextN  int                           // next host octet (172.16.0.<n>), starts at 2
	freed  []int                         // returned octets to reuse
	egress map[string][]proxy.EgressRule // srcIP → rules (merged into the proxy file)
	isoMu  sync.Mutex                    // serialize isolation-backend mutations across keys

	// pool preallocates a fixed set of sandbox network slots (wired netns/veth/
	// iptables + DNS sink) so a create claims one instead of paying that contended
	// setup on the request path. nil when -prealloc-pool is 0. Slots are reused:
	// released slots are reset (conntrack flush) and returned.
	pool *preallocPool

	// launchSem caps concurrent creates so a burst doesn't oversubscribe the
	// node's cores during the CPU-bound resume/convergence phases (firecracker
	// resume + guest re-IP/mount). A buffered channel of capacity
	// -max-concurrent-launches; nil disables the cap (unbounded). Held from the
	// start of createPacked until the sandbox is ready (or the create errors).
	launchSem chan struct{}
}

// allocIP hands out the next free pod-local IP (172.16.0.2 …).
func (p *packState) allocIP() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int
	if k := len(p.freed); k > 0 {
		n = p.freed[k-1]
		p.freed = p.freed[:k-1]
	} else {
		if p.nextN == 0 {
			p.nextN = 2
		}
		n = p.nextN
		p.nextN++
	}
	return fmt.Sprintf("172.16.%d.2", n), n
}

func (p *packState) freeIP(n int) {
	p.mu.Lock()
	p.freed = append(p.freed, n)
	p.mu.Unlock()
}

// claimSlot returns the IP/octet for a new sandbox, preferring a preallocated
// slot from the pool (prealloc=true — its netns/iptables/overlay are already
// wired, so RedirectEgress/MountRoot are no-ops on the request path). When the
// pool is disabled or empty it allocates and wires an octet synchronously, the
// historical path.
func (p *packState) claimSlot() (ip string, octet int, prealloc bool) {
	if p.pool != nil {
		if ip, octet, ok := p.pool.claim(); ok {
			return ip, octet, true
		}
	}
	ip, octet = p.allocIP()
	return ip, octet, false
}

// releaseSlot reclaims a sandbox's octet: a preallocated slot goes back to the
// pool (which resets and reuses it), a synchronously-wired one returns to the IP
// allocator.
func (p *packState) releaseSlot(octet int, prealloc bool) {
	if prealloc && p.pool != nil {
		p.pool.release(octet)
		return
	}
	p.freeIP(octet)
}

// egressReloadTimeout bounds how long the create path waits for sbxproxy to ack
// a coalesced egress reload (egressGate.waitApplied). The wait overlaps the
// snapshot resume + apply-resume-state work, so it virtually never elapses; it's
// only a safety net against a missed ack so a create can't hang forever.
const egressReloadTimeout = 3 * time.Second

// egressFile is the on-disk envelope pack mode writes to the shared sbxproxy
// rules file: the full per-source allowlist plus a monotonic generation that
// sbxproxy echoes back once it has applied this exact rule set (see egressGate).
// The boot/single-sandbox path writes a bare array instead; sbxproxy's readRules
// accepts both. The generation rides with its rules so an echoed generation
// always corresponds to the applied content.
type egressFile struct {
	Generation uint64                        `json:"generation"`
	Sources    map[string][]proxy.EgressRule `json:"sources"`
}

// egressGate coalesces egress-rule reloads between sandboxd and the shared
// sbxproxy. setEgress mutates the in-memory map, bumps `desired`, and wakes the
// reloader; a single reloader goroutine collapses a burst of changes into one
// rules-file rewrite + SIGHUP — turning the old O(N) writes+reloads+reparses per
// create/delete burst into ~one. sbxproxy echoes the generation it applied back
// over its events fd, advancing `applied`; the create path blocks (waitApplied)
// until applied >= its generation so a sandbox's ACL is enforced before it is
// marked ready. It is decoupled from packState so the proxy's event handler —
// wired before packState is built — can advance `applied`.
type egressGate struct {
	wake chan struct{} // buffered(1): non-blocking reloader wake

	mu        sync.Mutex
	desired   uint64
	applied   uint64
	appliedCh chan struct{} // closed+replaced each time `applied` advances (broadcast)
}

func newEgressGate() *egressGate {
	return &egressGate{wake: make(chan struct{}, 1), appliedCh: make(chan struct{})}
}

// bumpDesired records a new pending change and returns its generation. Callers
// then call signal() to wake the reloader and may waitApplied() on the result.
func (g *egressGate) bumpDesired() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.desired++
	return g.desired
}

func (g *egressGate) currentDesired() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.desired
}

// signal wakes the reloader without blocking; a pending wake already covers any
// number of coalesced changes (the reloader always reads the latest desired).
func (g *egressGate) signal() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// markApplied advances the applied generation (sbxproxy's echo) and broadcasts
// to waiters. Monotonic: out-of-order or duplicate echoes are ignored.
func (g *egressGate) markApplied(gen uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gen > g.applied {
		g.applied = gen
		close(g.appliedCh)
		g.appliedCh = make(chan struct{})
	}
}

// waitApplied blocks until sbxproxy confirms a generation >= gen, ctx is done,
// or egressReloadTimeout elapses (whichever first). A timeout/cancel returns
// without error: the pre-coalescing code applied rules with no readiness wait at
// all, so proceeding is never worse — at worst this one create loses the
// enforced-before-ready guarantee.
func (g *egressGate) waitApplied(ctx context.Context, gen uint64) {
	t := time.NewTimer(egressReloadTimeout)
	defer t.Stop()
	for {
		g.mu.Lock()
		if g.applied >= gen {
			g.mu.Unlock()
			return
		}
		ch := g.appliedCh
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return
		case <-t.C:
			log.Printf("sandboxd: pack: egress reload gen %d not acked within %s; proceeding", gen, egressReloadTimeout)
			return
		}
	}
}

// runEgressReloader is the single goroutine that owns the rules-file rewrite +
// SIGHUP. Woken via egressGate.wake, each pass writes the *latest* desired
// generation alongside the full per-source map, so a burst of setEgress calls
// converges to the final state in one reload regardless of burst size. The
// SIGHUP after the final write guarantees sbxproxy reads (and echoes) that
// generation, so every waiter <= it is released. Runs until ctx is cancelled.
func (p *packState) runEgressReloader(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.egressGate.wake:
		}
		gen := p.egressGate.currentDesired()
		p.mu.Lock()
		snapshot := make(map[string][]proxy.EgressRule, len(p.egress))
		maps.Copy(snapshot, p.egress)
		p.mu.Unlock()

		if err := writeJSON(p.rulesPath, egressFile{Generation: gen, Sources: snapshot}); err != nil {
			log.Printf("sandboxd: pack: write egress rules: %v", err)
			continue
		}
		if err := syscall.Kill(p.proxyPID, syscall.SIGHUP); err != nil {
			log.Printf("sandboxd: pack: SIGHUP sbxproxy: %v", err)
		}
	}
}

// setEgress records (or clears, when rules is nil) the egress allowlist for a
// source IP, then wakes the coalescing reloader. It returns the reload
// generation that includes this change; pass it to egressGate.waitApplied to
// block until sbxproxy has the rules live. Non-blocking itself, so a teardown
// (rules == nil) need not wait — a removed sandbox briefly still-allowed is
// harmless since its workload is already gone.
func (p *packState) setEgress(ip string, rules []proxy.EgressRule) uint64 {
	p.mu.Lock()
	if p.egress == nil {
		p.egress = map[string][]proxy.EgressRule{}
	}
	if rules == nil {
		delete(p.egress, ip)
	} else {
		p.egress[ip] = rules
	}
	p.mu.Unlock()

	gen := p.egressGate.bumpDesired()
	p.egressGate.signal()
	return gen
}

// createPacked brings up a new sandbox for key inside a pack pod: allocate an
// IP, build a per-key isolation instance (own netns/overlay/cgroup), mount its
// workspaces, register its egress, and launch the workload. Returns once the
// workload is ready.
// setupEntrypointTTY publishes the resumed sandbox's entrypoint terminal when the
// config requested a tty override. The backend returns a host bridge command (the
// microvm runs the override as a tty exec session in a guest pty; the container
// backend returns nil, having wired its entrypoint pty at launch). The bridge is
// wrapped in a pty.Session and published via SetEntrypointTTY so /v1/exec-stream
// with an empty command attaches to it. When the session ends — the override
// exited (pty EOF) or the sandbox was torn down — onExit cancels the lifecycle so
// the override's exit ends the sandbox, mirroring an entrypoint exit. A no-op when
// the backend returns no bridge (no tty override).
func setupEntrypointTTY(ctx context.Context, key string, iso isolation.Isolation, sb *handlers.Sandbox, onExit context.CancelFunc) {
	bridge, cleanup, err := iso.EntrypointTTYBridge(ctx)
	if err != nil {
		log.Printf("sandboxd: %s: entrypoint tty bridge: %v", key, err)
		return
	}
	if bridge == nil {
		return
	}
	master, err := pty.Start(bridge)
	if err != nil {
		cleanup()
		log.Printf("sandboxd: %s: start entrypoint tty: %v", key, err)
		return
	}
	sess := pty.NewSession(master, nil)
	sb.SetEntrypointTTY(sess)
	log.Printf("sandboxd: %s: entrypoint attached to tty (microvm bridge)", key)
	go func() {
		<-sess.Done()
		sess.Close()
		cleanup()
		onExit()
	}()
}

func (s *supervisor) createPacked(ctx context.Context, key string, cfg gen.SandboxConfig) (*handlers.Sandbox, error) {
	p := s.pack
	sp, err := specFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Cap concurrent creates (-max-concurrent-launches) so a burst doesn't
	// oversubscribe the node's cores during the CPU-bound resume/convergence
	// phases. Held until this create returns (sandbox ready, or errored); the
	// running sandbox does not hold a slot. Abort if the request is cancelled
	// while queued.
	if p.launchSem != nil {
		select {
		case p.launchSem <- struct{}{}:
			defer func() { <-p.launchSem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Time each stage of bringing this packed sandbox up — especially the resume
	// fast path (snapshot load + apply-resume-state) — the same way boot phases
	// time the cold path, so a slow or failing claim can be attributed.
	phase := &bootPhase{last: time.Now()}

	ip, octet, prealloc := p.claimSlot()
	// The sandbox runs on the pod lifecycle, with its own cancel for DELETE.
	sbCtx, cancel := context.WithCancel(p.ctx)
	cleanup := func() { cancel() }

	// A packed microvm resumes from a per-sandbox VM snapshot when the config names
	// one (snapshot.vm.key) whose state dir under -snapshot-dir already holds a
	// snapshot — captured earlier via the snapshot action. Otherwise it cold-boots
	// into that state dir (its overlay lives there, the source of truth), so a later
	// snapshot captures in place. Guest sizing comes from this sandbox's config;
	// isolation.New applies its defaults when cpu/memory are unset.
	stateDir, stateEphemeral := "", false
	if p.isoKind == isolation.KindMicroVM {
		stateDir, stateEphemeral = vmStateDir(p.snapshotDir, sp)
	}

	iso, err := isolation.New(p.isoKind, isolation.Config{
		Key:              key,
		GuestIP:          ip,
		Hostname:         p.hostname,
		VMStateDir:       stateDir,
		VMStateEphemeral: stateEphemeral,
		LocalMounts:      isolationLocalMounts(sp.FS),
		VcpuCount:        ceilVcpu(sp.CPU),
		MemoryMiB:        intOrZero(sp.Memory),
		Prealloc:         prealloc,
	})
	if err != nil {
		cleanup()
		p.releaseSlot(octet, prealloc)
		return nil, err
	}
	if err := iso.MountRoot(); err != nil {
		cleanup()
		p.releaseSlot(octet, prealloc)
		return nil, fmt.Errorf("mount root: %w", err)
	}
	if len(p.caData) > 0 {
		if err := iso.InstallCA(p.caData); err != nil {
			log.Printf("sandboxd: pack %q: install CA: %v", key, err)
		}
	}

	phase.mark("pack " + key + ": isolation root + CA")

	broker := events.New(events.DefaultCapacity, 0)
	store := api.NewConfigStore(cfg)

	// Per-key sbxfuse workspaces under /run/sandboxd/<key> (host side); the guest
	// still sees each at its configured mount (e.g. /workspace).
	keyRoot := filepath.Join("/run/sandboxd", key)
	mountMgr := newMountManager(sbCtx, p.children, broker, iso, &p.isoMu, p.fuse, p.workDir, p.snapshotDir, p.soMark)
	mountMgr.keyPrefix = keyRoot
	mountMgr.SetRootMounted()
	if err := mountMgr.Reconcile(sp); err != nil {
		cleanup()
		p.releaseSlot(octet, prealloc)
		return nil, fmt.Errorf("workspaces: %w", err)
	}
	phase.mark("pack " + key + ": workspace mount")

	// Egress: bring up the sandbox's netns/veth + host REDIRECT, then register its
	// allowlist under its source IP so the shared sbxproxy enforces it per-source.
	// RedirectEgress builds this sandbox's netns/veth/tap (netlink, no per-command
	// iproute2 fork) and loads its firewall rules with a batched iptables-restore
	// per netns-context (see setupPackedNetMicrovm) — a handful of execs, not the
	// per-rule fork chain the early implementation paid. The setEgress reload is
	// coalesced + async (the reloader batches the file write + SIGHUP), so this mark
	// attributes the egress cost to the netns/tap setup, separate from the
	// workspace/root phases. The returned generation is awaited later
	// (egressGate.waitApplied, before the sandbox is marked ready) so the reload
	// overlaps the snapshot resume instead of serializing N reloads on the request
	// path.
	if err := iso.RedirectEgress(sbCtx, p.proxyPort, p.dnsPort, p.soMark); err != nil {
		cleanup()
		p.releaseSlot(octet, prealloc)
		return nil, fmt.Errorf("egress: %w", err)
	}
	egressGen := p.setEgress(ip, sp.Egress)
	phase.mark("pack " + key + ": egress (netns + iptables)")
	// Now the per-VM netns exists, bring up the host-side 9p workspace listeners
	// (deferred at ExportWorkspace, which ran before the netns). A cold-booting guest
	// mounts its workspaces from the params drive during boot, so the listeners must
	// be live before the VMM starts below. No-op for containers; the resume path also
	// binds these via ApplyResumeState. Without this a cold-booted packed microvm has
	// no workspace serving (no fs.request/fs.response events).
	if err := iso.BindWorkspaces(sbCtx); err != nil {
		cleanup()
		p.releaseSlot(octet, prealloc)
		return nil, fmt.Errorf("bind workspaces: %w", err)
	}
	// Route sbxproxy audit events for this source IP to the sandbox's own broker.
	p.router.register(ip, broker)

	// Container vs microvm differ in how the workload's DNS + filesystem are wired:
	//
	//   - container: runs in its own netns on the pod bridge, so it needs a DNS
	//     sink bound to its own gateway:53 (a DNAT'd sink would need fragile
	//     cross-netns conntrack un-NAT), its workspaces bind-mounted from the host
	//     FUSE dirs, and an /etc/resolv.conf pointed at its bridge gateway.
	//   - microvm: its tap lives in the pod netns, so RedirectEgress DNATs guest
	//     DNS straight to the shared 127.0.0.1:dnsPort sink, and LaunchAgent builds
	//     /etc/hosts + a gateway-pointed resolv.conf into the params drive while
	//     workspaces are served over 9p (ExportWorkspace). So none of the host-side
	//     binds/sink below apply — the guest gets an empty bind set.
	var mounts []runc.BindMount
	if p.isoKind == isolation.KindContainer {
		// DNS sink bound directly to this sandbox's gateway:53 (the address its guest
		// queries). Answering from the bound address avoids the fragile cross-netns
		// conntrack un-NAT a DNAT'd sink would need. The guest then connects to the
		// placeholder, which the TCP DNAT funnels to sbxproxy. dnsSinkIP matches
		// sbxproxy's -dns-sink default (TEST-NET-1).
		gw := fmt.Sprintf("172.16.%d.1", octet)
		if pc, err := net.ListenPacket("udp", gw+":53"); err != nil {
			log.Printf("sandboxd: pack %q: dns sink on %s:53: %v", key, gw, err)
		} else {
			go proxy.ServeSink(sbCtx, pc, net.ParseIP("192.0.2.1"), nil)
		}

		// Bind the per-key host workspace dirs to their guest mount paths.
		mounts = make([]runc.BindMount, 0, len(sp.FS)+2)
		for i := range sp.FS {
			if sp.FS[i].Internal {
				continue
			}
			mounts = append(mounts, runc.BindMount{Source: mountMgr.hostMount(sp.FS[i]), Destination: sp.FS[i].Mount, Options: []string{"rw"}})
		}
		// A packed sandbox is in its own netns, so the host's resolv.conf (docker's
		// loopback resolver 127.0.0.11) is dead there — its DNS would never leave the
		// netns to hit the REDIRECT. Point it at the bridge gateway instead, so DNS
		// forwards out the veth and is sinkholed by the host PREROUTING rule.
		resolvPath := filepath.Join(keyRoot, "resolv.conf")
		gateway := fmt.Sprintf("172.16.%d.1", octet)
		if err := os.WriteFile(resolvPath, []byte("nameserver "+gateway+"\n"), 0o644); err != nil {
			cleanup()
			p.releaseSlot(octet, prealloc)
			return nil, fmt.Errorf("write resolv.conf: %w", err)
		}
		mounts = append(mounts,
			runc.BindMount{Source: "/etc/hosts", Destination: "/etc/hosts", Options: []string{"ro"}},
			runc.BindMount{Source: resolvPath, Destination: "/etc/resolv.conf", Options: []string{"ro"}},
		)
	}

	imgCfg := *p.imgCfg

	// A tty entrypoint (e.g. node's REPL, an interactive shell) stays alive only
	// when attached to a real terminal; launched on plain pipes it reads EOF on
	// stdin and exits immediately. The container backend wraps the entrypoint in a
	// host pty at launch; the microvm backend runs the override as a tty exec
	// session in a guest pty (EntrypointTTYBridge) — which needs an actual override
	// (attaching to the image's default keepalive is useless, so drop tty then).
	// Decide tty against the effective entrypoint (override if set, else default).
	effCfg := imgCfg
	if len(sp.Entrypoint) > 0 {
		effCfg.Entrypoint = sp.Entrypoint
		effCfg.Cmd = nil
	}
	ttyEnabled := sp.Tty != nil && *sp.Tty
	if ttyEnabled && entrypointIsTail(&effCfg) {
		ttyEnabled = false
	}
	// For a microvm, a tty entrypoint override runs as a host-driven guest tty exec
	// session (so /v1/exec-stream can attach), NOT as the guest's console workload —
	// the console keeps the image default keepalive. So don't bake the override into
	// the params drive in that case; record it for EntrypointTTYBridge instead (at
	// launch, below). Every other case bakes the override as the console workload.
	ttyExecEntrypoint := ttyEnabled && len(sp.Entrypoint) > 0 && p.isoKind == isolation.KindMicroVM
	if len(sp.Entrypoint) > 0 && !ttyExecEntrypoint {
		imgCfg.Entrypoint = sp.Entrypoint
		imgCfg.Cmd = nil
	}
	if sp.Cwd != "" && !ttyExecEntrypoint {
		imgCfg.WorkingDir = sp.Cwd
	}
	agentEnv := make(map[string]string, len(sp.Env)+1)
	maps.Copy(agentEnv, sp.Env)
	agentEnv["NODE_EXTRA_CA_CERTS"] = isolation.NodeCACertPath
	if ttyEnabled {
		if _, ok := agentEnv["TERM"]; !ok {
			agentEnv["TERM"] = "xterm-256color"
		}
		if _, ok := agentEnv["COLORTERM"]; !ok {
			agentEnv["COLORTERM"] = "truecolor"
		}
	}

	// Launch the workload. Three shapes:
	//   - microvm resume: a fresh VMM loads the shared base snapshot, then the
	//     control RPC (below) delivers env/workspaces/re-IP. The VMM runs on its
	//     own context so teardown can sync the guest before killing it.
	//   - microvm cold boot: the VMM boots the entrypoint (base build failed/absent).
	//   - container: runc launches the entrypoint, optionally on a pty.
	var (
		agentCmd      *exec.Cmd
		agentDone     <-chan struct{}
		entrypointTTY *pty.Session
		stopWorkload  = func() {}                                  // extra stop step (microvm: cancel the VMM ctx)
		flushAgent    = func(context.Context) error { return nil } // microvm: sync the guest before stop
		resumed       bool
	)
	if p.isoKind == isolation.KindMicroVM {
		// The VMM is supervised on vmCtx (not sbCtx) so the teardown goroutine can
		// FlushAgent the still-running guest before stopWorkload kills it.
		vmCtx, cancelVM := context.WithCancel(context.Background())
		stopWorkload = cancelVM
		flushAgent = iso.FlushAgent
		failVM := func(format string, a ...any) (*handlers.Sandbox, error) {
			cancelVM()
			cleanup()
			p.releaseSlot(octet, prealloc)
			return nil, fmt.Errorf(format, a...)
		}
		var vmBin string
		var vmArgs []string
		if iso.HasPrewarmSnapshot() {
			resumed = true
			vmBin, vmArgs, err = iso.ResumeAgent()
		} else {
			vmBin, vmArgs, err = iso.LaunchAgent(isolation.AgentConfig{ImageConfig: &imgCfg, Env: agentEnv, Hostname: p.hostname})
		}
		if err != nil {
			return failVM("prepare agent: %w", err)
		}
		cmd, done, startErr := startChild(vmCtx, p.children, "sandbox:"+key, vmBin, vmArgs, nil, nil, nil)
		if startErr != nil {
			return failVM("start agent: %w", startErr)
		}
		agentCmd, agentDone = cmd, done
		if resumed {
			// Load the base snapshot into the fresh VMM (with this VM's tap override).
			resumeCtx, cancelResume := context.WithTimeout(sbCtx, snapshotResumeTimeout)
			rErr := iso.ResumeReady(resumeCtx)
			cancelResume()
			if rErr != nil {
				return failVM("resume snapshot: %w", rErr)
			}
			phase.mark("pack " + key + ": snapshot resume")
		}
	} else {
		agentBin, agentArgs, lerr := iso.LaunchAgent(isolation.AgentConfig{
			ImageConfig: &imgCfg,
			Env:         agentEnv,
			Mounts:      mounts,
			Hostname:    p.hostname,
			TTY:         ttyEnabled,
		})
		if lerr != nil {
			cleanup()
			p.releaseSlot(octet, prealloc)
			return nil, fmt.Errorf("prepare agent: %w", lerr)
		}
		// On the tty path the entrypoint runs attached to a pty whose session backs
		// exec-stream attach requests (published below via SetEntrypointTTY); its
		// master EOF is the "agent exited" signal. Otherwise launch on pipes.
		if ttyEnabled {
			cmd, sess, ttyErr := startAgentTTY(sbCtx, agentBin, agentArgs)
			if ttyErr != nil {
				cleanup()
				p.releaseSlot(octet, prealloc)
				return nil, fmt.Errorf("start agent (tty): %w", ttyErr)
			}
			agentCmd, entrypointTTY, agentDone = cmd, sess, sess.Done()
		} else {
			cmd, done, startErr := startChild(sbCtx, p.children, "sandbox:"+key, agentBin, agentArgs, nil, nil, publishAgentStdio(broker))
			if startErr != nil {
				cleanup()
				p.releaseSlot(octet, prealloc)
				return nil, fmt.Errorf("start agent: %w", startErr)
			}
			agentCmd, agentDone = cmd, done
		}
	}

	sb := handlers.NewSandbox(key, p.soMark)
	// A packed sandbox runs in its own netns; its workload is reachable from the
	// pod netns only at the guest IP, not 127.0.0.1. Point the ingress reverse
	// proxy there so /proxy/<port> reaches this key's workload (and only this one).
	sb.SetProxyHost(ip)
	sb.SetIsolation(iso)
	sb.SetSnapshotDir(p.snapshotDir)
	sb.SetBroker(broker)
	sb.SetStore(store)
	// Tie exec sessions to this sandbox's lifecycle: a DELETE (sbCtx cancel)
	// kills in-flight execs, so the microvm bridge (sbxvsock) doesn't linger
	// blocked on a read to the guest the teardown is about to kill.
	sb.SetLifecycleContext(sbCtx)
	// Publish the entrypoint pty (tty path only) so exec-stream attach requests
	// reach the running entrypoint terminal.
	if entrypointTTY != nil {
		sb.SetEntrypointTTY(entrypointTTY)
	}
	lifetime := api.NewLifetime(func() time.Duration {
		c, err := store.Get()
		if err != nil || c.Ttl == nil {
			return defaultTtl
		}
		return time.Duration(*c.Ttl) * time.Second
	}, cancel)
	sb.SetLifetime(lifetime)
	broker.SetActivityHook(lifetime.Reset)

	s.mu.Lock()
	s.sandboxes[key] = sb
	s.cancels[key] = cancel
	if s.image == "" {
		s.image = specImage(sp)
	}
	s.mu.Unlock()
	s.lifecycle.publish(key, gen.PodEventStatusStarting)

	// Teardown: on DELETE (cancel), the agent exiting, or pod shutdown, stop the
	// workload and free the slot (netns + overlay via UnmountRoot, IP, egress,
	// map entry). The shared sbxfuse process outlives this sandbox, so its
	// workspaces must be unmounted explicitly via stopAll (cancelling sbCtx no
	// longer reaches them as it did with per-sandbox sbxfuse daemons).
	go func() {
		select {
		case <-sbCtx.Done():
		case <-agentDone:
			cancel()
		}
		sb.SetStopping() // reflect teardown in the listing (covers agent-exit, not just DELETE)
		s.lifecycle.publish(key, gen.PodEventStatusStopping)
		stopCtx, c := context.WithTimeout(context.Background(), fsDrainTimeout)
		// microvm: sync the guest so its overlay writes are durable for the snapshot
		// capture below, then drop the VMM (its ctx is separate from sbCtx).
		// container: both are no-ops; StopAgent kills+deletes the container.
		_ = flushAgent(stopCtx)
		_ = iso.StopAgent(stopCtx)
		stopWorkload()
		c()
		if agentCmd != nil {
			_ = agentCmd.Wait() // reap the launched workload process
		}
		// Capture snapshot before unmounting — mirrors finalizeShutdown for the
		// boot sandbox. Must run while the overlay and FUSE mounts are still up.
		if cfg, err := store.Get(); err != nil {
			log.Printf("sandboxd: pack %q: snapshot: read config: %v", key, err)
		} else if sn := cfg.Snapshot; sn != nil && sn.Files != nil {
			f := sn.Files
			if f.Key != "" && f.WriteOnShutdown != nil && *f.WriteOnShutdown {
				dir := p.snapshotDir
				if f.Mount != nil && *f.Mount != "" {
					dir = *f.Mount
				}
				if dir != "" {
					var include []string
					if f.Include != nil {
						include = *f.Include
					}
					dst := snapshot.SnapshotPath(dir, f.Key)
					log.Printf("sandboxd: pack %q: snapshot: capturing %v → %s", key, include, dst)
					if err := iso.CaptureSnapshot(dst, include); err != nil {
						log.Printf("sandboxd: pack %q: snapshot capture: %v", key, err)
					}
				}
			}
		}
		mountMgr.stopAll()
		// Free the reusable network slot + overlay/netns/IP *before* clearing the
		// per-key workspace tree below, so a still-wedged sbxfuse mountpoint can never
		// leak them. A leaked pool slot is the costly failure here: it permanently
		// shrinks the warm pool and pushes later creates onto the slow synchronous
		// path. UnmountRoot (netns for non-prealloc, dm-snapshot overlay, jail dir)
		// and the slot release don't depend on the per-key workspace dir, so they run
		// first and unconditionally.
		_ = iso.UnmountRoot()
		p.router.unregister(ip)
		p.setEgress(ip, nil)
		p.releaseSlot(octet, prealloc)
		// Remove this key's host-side workspace tree (mountpoints + backend dirs);
		// the shared sbxfuse outlives this sandbox, so the per-key dir under
		// /run/sandboxd otherwise leaks for the pod's lifetime and blocks a same-key
		// re-create. clearStaleMount lazy-detaches (MNT_DETACH, non-blocking) any
		// FUSE mountpoints still under keyRoot *before* RemoveAll — covering both the
		// async-unmount-not-yet-landed case and a genuinely wedged mount. The former
		// order (RemoveAll first, force-detach only after 50 failures) let a single
		// wedged unlink syscall block the whole teardown before the force-detach was
		// ever reached — stalling it before the slot release above and leaking the
		// netns/overlay/IP/slot.
		if err := clearStaleMount(keyRoot); err != nil {
			log.Printf("sandboxd: pack %q: clear workspace dir %s: %v", key, keyRoot, err)
		}
		s.mu.Lock()
		delete(s.sandboxes, key)
		delete(s.cancels, key)
		s.mu.Unlock()
		s.lifecycle.publish(key, gen.PodEventStatusStopped)
		log.Printf("sandboxd: pack %q: torn down (ip=%s)", key, ip)
	}()

	go lifetime.Run(sbCtx)

	if resumed {
		// The resumed guest is already past its readiness beacon (the entrypoint is
		// running in the loaded snapshot), so instead of WaitReady deliver this
		// sandbox's env + workspaces + re-IP over the control channel. The guest is
		// marked ready only once this converges (self-healing retry inside).
		envSlice := make([]string, 0, len(agentEnv))
		for k, v := range agentEnv {
			envSlice = append(envSlice, k+"="+v)
		}
		rs := isolation.ResumeState{Env: envSlice, MountWorkspacesSync: true}
		// The snapshot restores the workload it captured running — the config's own
		// entrypoint, captured at cold-boot. We do NOT relaunch it: a non-tty
		// entrypoint comes back on the guest console (relaunching would duplicate the
		// workload), and changing the entrypoint/cwd of a resumed (already-running)
		// workload is a no-op, like changing it on any started sandbox.
		//
		// A tty entrypoint is the one exception: it ran as a guest tty exec session
		// whose host-side pty bridge does not survive a snapshot/restore, so re-establish
		// it below (setupEntrypointTTY runs the recorded override as a fresh session).
		if ttyEnabled && len(sp.Entrypoint) > 0 {
			iso.PrepareEntrypointTTY(sp.Entrypoint, sp.Cwd, envSlice)
		}
		if err := iso.ApplyResumeState(sbCtx, rs); err != nil {
			cleanup()
			return nil, fmt.Errorf("apply resume state: %w", err)
		}
		phase.mark("pack " + key + ": apply resume state")
		setupEntrypointTTY(sbCtx, key, iso, sb, cancel)
	} else {
		if err := iso.WaitReady(sbCtx); err != nil {
			cleanup()
			return nil, fmt.Errorf("wait ready: %w", err)
		}
		// Cold boot: a microvm tty entrypoint override runs as a host-driven guest
		// tty exec session (the guest console runs the image default keepalive), so
		// record it and publish the bridge for /v1/exec-stream attach — its exit ends
		// the sandbox. No-op when there's no such override (ttyExecEntrypoint false).
		if ttyExecEntrypoint {
			envSlice := make([]string, 0, len(agentEnv))
			for k, v := range agentEnv {
				envSlice = append(envSlice, k+"="+v)
			}
			iso.PrepareEntrypointTTY(sp.Entrypoint, sp.Cwd, envSlice)
			setupEntrypointTTY(sbCtx, key, iso, sb, cancel)
		}
	}
	// Gate readiness on the egress reload landing in sbxproxy. The reload was
	// kicked off (coalesced) right after RedirectEgress and runs concurrently
	// with the resume/apply-resume-state work above, so this almost always
	// returns immediately — it only blocks in the rare case the reload hasn't
	// caught up, guaranteeing this sandbox's ACL is enforced before its workload
	// is reachable. Bounded by egressReloadTimeout (proceeds on timeout/cancel).
	p.egressGate.waitApplied(sbCtx, egressGen)
	go api.PollResourceUsage(sbCtx, broker, iso.CgroupPath())
	// The workload is now running; a subsequent config-apply that adds a
	// workspace must inject it into the live workload via ApplyResumeState.
	mountMgr.SetWorkloadLive()
	sb.SetStarted()
	sb.NotifyReady()
	s.lifecycle.publish(key, gen.PodEventStatusRunning)
	log.Printf("sandboxd: pack %q: ready (ip=%s)", key, ip)

	// Watch for runtime config-apply events and reconcile FS mounts + egress.
	// The non-pack path does this via reconcileSidecars; here we inline the
	// equivalent so egress uses p.setEgress (per-source rules) instead of the
	// single-sandbox rules file + SIGHUP path.
	go func() {
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
					log.Printf("sandboxd: pack %q: reconcile: %v", key, err)
					continue
				}
				desiredSpec, err := specFromConfig(cfg)
				if err != nil {
					log.Printf("sandboxd: pack %q: reconcile: %v", key, err)
					continue
				}
				p.setEgress(ip, desiredSpec.Egress)
				if err := mountMgr.Reconcile(desiredSpec); err != nil {
					log.Printf("sandboxd: pack %q: reconcile fs: %v", key, err)
				}
				log.Printf("sandboxd: pack %q: reconciled from config (event id=%d)", key, entry.ID)
			case <-sbCtx.Done():
				return
			}
		}
	}()

	return sb, nil
}
