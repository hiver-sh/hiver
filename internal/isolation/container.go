package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hiver-sh/hiver/internal/runc"
	"github.com/hiver-sh/hiver/internal/snapshot"
	"github.com/hiver-sh/hiver/internal/spec"
)

// wsMount pairs the host-side FUSE mount path with the guest-visible path.
// They are the same for the boot (non-packed) sandbox but differ for packed
// sandboxes, where the host path is namespaced under /run/sandboxd/<key>/mnt/.
type wsMount struct {
	host  string // path reachable in sandboxd's mount namespace
	guest string // destination inside the agent container
}

// container is the runc-backed Isolation. All primitives are host-level
// operations the container shares through namespaces: overlayfs and FUSE
// mounts live in the sandbox-pod mount namespace, iptables in its (shared)
// network namespace, and exec is `runc exec` into the agent container.
type container struct {
	// containerID is the id runc assigns the agent. sandboxd is PID 1 in
	// the pod, so this is stable per sandbox ("agent-1"); deriving it from
	// the pid keeps it in lockstep with the value sandboxd's launcher uses.
	containerID string
	// cgroupPath is the absolute cgroup the agent runs under, derived from
	// the pod hostname so sandboxes sharing a host don't collide. runc places
	// the agent here via the bundle config; PollResourceUsage reads it.
	cgroupPath string
	// localMounts are the local-backend FUSE workspaces snapshot routes to.
	localMounts []SnapshotMount
	// vcpuCount and memSizeMib are the compute allocation enforced on the
	// agent via the runc bundle's linux.resources (CPU quota + memory limit).
	vcpuCount  int
	memSizeMib int
	// overlay/bundleDir/readyFifoPath locate this sandbox's writable runtime
	// state. They derive from a per-sandbox root (/mnt for the boot sandbox,
	// /run/sandboxd/<key>/rt for a packed one) so multiple sandboxes can share
	// one pod without colliding; the image rootfs (overlay.Lower) stays shared.
	overlay       runc.Overlay
	bundleDir     string
	readyFifoPath string

	// guestIP/netnsName are set for a packed sandbox: it runs in its own network
	// namespace (netnsName, at /var/run/netns/<netnsName>) with guestIP on the
	// pod bridge, so its egress has a distinct source IP. Empty for the boot
	// sandbox, which shares the pod netns.
	guestIP   string
	netnsName string

	// prealloc marks a sandbox claiming a preallocated slot whose netns/veth/
	// iptables were wired ahead of time by the pod's prealloc pool
	// (Config.Prealloc). RedirectEgress is then a no-op and UnmountRoot leaves the
	// netns teardown to the pool.
	prealloc bool

	// wsMountRoot/wsBackendRoot are the per-key host roots a packed sandbox's
	// sbxfuse workspaces live under (/run/sandboxd/<key>/{mnt,backend}); the file
	// API (Files) resolves workspace paths there instead of the default
	// <mount>-backend. Empty for the boot sandbox (host==guest layout).
	wsMountRoot   string
	wsBackendRoot string

	// readyFifo is the read end of readyFifoPath, opened O_RDWR by LaunchAgent so
	// the container's poststart hook never blocks writing to it. WaitReady reads
	// the hook's byte from here. nil until LaunchAgent runs.
	readyFifo *os.File

	// prewarmCmd is the backgrounded `runc run` process when the container was
	// started by PrewarmSnapshot (the entrypoint runs warm before claim and the
	// claim adopts it). nil on the cold path. StopAgent reaps it on teardown.
	prewarmCmd *exec.Cmd
	// hasPrewarm is set once PrewarmSnapshot brought the entrypoint container up;
	// it routes the claim through the resume path instead of a cold launch.
	hasPrewarm bool

	// mu guards workspaces/bound. workspaces is every mount ExportWorkspace has
	// recorded; bound marks the ones the agent can already see (runc bind-mounts
	// them at launch). MountWorkspacesLive injects the difference — workspaces
	// added by a runtime config-apply — into the running container.
	mu         sync.Mutex
	workspaces []wsMount
	bound      map[string]bool
}

func newContainer(cfg Config) *container {
	// The boot sandbox (no Key) keeps the historical /mnt layout and a
	// pid-derived container id, so its behavior is unchanged. A packed sandbox
	// (Key set) gets its own root, container id, and cgroup so N can coexist in
	// one pod. The image rootfs (overlay.Lower) is shared either way.
	root := runc.MntDir
	containerID := fmt.Sprintf("agent-%d", os.Getpid())
	cgroupName := cfg.Hostname
	if cfg.Key != "" {
		root = filepath.Join("/run/sandboxd", cfg.Key, "rt")
		containerID = "agent-" + cfg.Key
		cgroupName = cfg.Hostname + "-" + cfg.Key
	}
	scratch := filepath.Join(root, "scratch")
	c := &container{
		containerID:   containerID,
		cgroupPath:    sandboxCgroupPath(cgroupName),
		localMounts:   cfg.LocalMounts,
		vcpuCount:     cfg.VcpuCount,
		memSizeMib:    cfg.MemoryMiB,
		overlay:       runc.Overlay{Lower: runc.RootfsDir, Scratch: scratch, Upper: filepath.Join(scratch, "upper"), Work: filepath.Join(scratch, "work"), Merged: filepath.Join(root, "merged")},
		bundleDir:     root,
		readyFifoPath: filepath.Join(scratch, "ready.fifo"),
	}
	// Packed sandbox: run in its own netns, named by its index (the 3rd octet of
	// 172.16.<n>.2) so the veth/netns names stay short (keys can be up to 64
	// chars; veth names ≤15).
	if cfg.GuestIP != "" {
		c.guestIP = cfg.GuestIP
		c.netnsName = "hsbx" + netID(cfg.GuestIP)
		c.prealloc = cfg.Prealloc
	}
	if cfg.Key != "" {
		// Matches the mountManager's keyPrefix layout (cmd/sandboxd): host
		// workspace dirs live under /run/sandboxd/<key>/{mnt,backend}/<slug>.
		c.wsMountRoot = filepath.Join("/run/sandboxd", cfg.Key, "mnt")
		c.wsBackendRoot = filepath.Join("/run/sandboxd", cfg.Key, "backend")
	}
	return c
}

// netID returns a packed sandbox's index — the 3rd octet of its guest IP
// (172.16.<n>.2 → "<n>") — used to name its netns and veth pair.
func netID(guestIP string) string {
	parts := strings.Split(guestIP, ".")
	if len(parts) == 4 {
		return parts[2]
	}
	return strings.ReplaceAll(guestIP, ".", "")
}

func (c *container) Kind() Kind { return KindContainer }

func (c *container) MountRoot() error { return runc.MountOverlay(c.overlay) }

func (c *container) UnmountRoot() error {
	// A prewarmed sandbox's netns is owned by the pod's prewarm pool, which tears
	// it down and refills the slot; tearing it down here would race that.
	if c.netnsName != "" && !c.prealloc {
		c.teardownPackedNet(context.Background())
	}
	return runc.UnmountOverlay(c.overlay)
}

// netnsPath is the runc OCI network-namespace path for a packed sandbox, or ""
// for the boot sandbox (which shares the pod netns).
func (c *container) netnsPath() string {
	if c.netnsName == "" {
		return ""
	}
	return "/var/run/netns/" + c.netnsName
}

// SeedWorkspace moves image-supplied content at rootfsMount (e.g. files a
// Dockerfile COPY'd to the mount path) into the FUSE backend dir so the agent
// sees them through the mount. Move (not copy) — the bind/overlay over the
// mount hides whatever's left in the rootfs anyway. It's a plain host-side
// filesystem move (sbxfuse runs on the host for both backends), so sandboxd
// calls it directly rather than through the Isolation interface.
func SeedWorkspace(backendDir, rootfsMount string) error {
	entries, err := os.ReadDir(rootfsMount)
	if err != nil {
		return nil // nothing to seed
	}
	for _, e := range entries {
		src := filepath.Join(rootfsMount, e.Name())
		dst := filepath.Join(backendDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			if !errors.Is(err, syscall.EXDEV) {
				return fmt.Errorf("seed %s → %s: %w", src, dst, err)
			}
			// Cross-device: fall back to copy + remove.
			if out, err := exec.Command("cp", "-a", src, dst).CombinedOutput(); err != nil {
				return fmt.Errorf("seed cp %s → %s: %w (%s)", src, dst, err, out)
			}
			if err := os.RemoveAll(src); err != nil {
				return fmt.Errorf("seed rm %s: %w", src, err)
			}
		}
	}
	return nil
}

// ExportWorkspace records the workspace. runc bind-mounts the ones present at
// launch from the bundle (see LaunchAgent); one added later by a config-apply is
// injected into the running container by MountWorkspacesLive.
func (c *container) ExportWorkspace(ctx context.Context, hostMount, guestMount string) error {
	c.mu.Lock()
	c.workspaces = append(c.workspaces, wsMount{host: hostMount, guest: guestMount})
	c.mu.Unlock()
	return nil
}

// UnexportWorkspace reverses ExportWorkspace for a mount dropped by a runtime
// config-apply. It forgets the workspace so a later resume can't re-bind it and,
// when the mount was already injected into the running container, detaches it from
// the container's mount namespace so the agent stops seeing it. Stopping the host
// sbxfuse daemon (done by the caller) only unmounts the host side; the container
// holds an independent clone bindWorkspaceIntoContainer move_mount'd into it.
//
// A workspace that was never bound — the container hasn't launched yet (prewarm),
// so runc will simply omit it from the launch bundle — only needs forgetting;
// there is nothing in a mount namespace to remove.
func (c *container) UnexportWorkspace(ctx context.Context, mount string) error {
	c.mu.Lock()
	var hostMount string
	found := false
	kept := make([]wsMount, 0, len(c.workspaces))
	for _, m := range c.workspaces {
		if m.guest == mount {
			hostMount = m.host
			found = true
			continue
		}
		kept = append(kept, m)
	}
	c.workspaces = kept
	wasBound := found && c.bound[hostMount]
	delete(c.bound, hostMount)
	c.mu.Unlock()

	if !wasBound {
		return nil
	}
	pid, err := c.agentPID()
	if err != nil {
		return fmt.Errorf("unexport workspace %s: %w", mount, err)
	}
	if err := unmountWorkspaceFromContainer(pid, mount); err != nil {
		return fmt.Errorf("unexport workspace %s: %w", mount, err)
	}
	log.Printf("isolation: unmounted workspace %s from running container (pid %d)", mount, pid)
	return nil
}

// InstallCA splices the sandbox CA (PEM) into the agent rootfs trust store
// so the agent validates the leaf certs sbxproxy mints during interception,
// and writes a standalone copy for NODE_EXTRA_CA_CERTS. Best-effort on the
// bundle: an image without ca-certificates gets a warning, since its TLS
// would fail regardless.
func (c *container) InstallCA(certPEM []byte) error {
	merged := c.overlay.Merged
	bundle := filepath.Join(merged, "etc/ssl/certs/ca-certificates.crt")
	if existing, err := os.ReadFile(bundle); err == nil {
		out := append(append(existing, '\n'), certPEM...)
		if err := os.WriteFile(bundle, out, 0o644); err != nil {
			return fmt.Errorf("install CA into %s: %w", bundle, err)
		}
	}
	node := filepath.Join(merged, NodeCACertPath)
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(node, certPEM, 0o644); err != nil {
		return err
	}
	// NSS clients (Chromium/Playwright) consult neither the system bundle nor
	// NODE_EXTRA_CA_CERTS — they read the per-user db at $HOME/.pki/nssdb. We
	// build that db host-side (certutil ships in the core image) and copy it into
	// the merged rootfs, so the agent image needs no NSS tooling. Best-effort: a
	// failure here only affects NSS clients, and most images don't run any.
	if files, err := buildNSSDB(certPEM); err != nil {
		log.Printf("install NSS CA: %v (NSS clients won't trust the sandbox CA)", err)
	} else {
		nssdb := filepath.Join(merged, workloadHome(merged), ".pki", "nssdb")
		if err := writeNSSDB(nssdb, files); err != nil {
			log.Printf("install NSS CA: write %s: %v", nssdb, err)
		}
	}
	return nil
}

// workloadHome returns the home directory of uid 0 from the rootfs's
// /etc/passwd, defaulting to /root when it can't be determined.
func workloadHome(merged string) string {
	data, err := os.ReadFile(filepath.Join(merged, "etc/passwd"))
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

// RedirectEgress installs the OUTPUT-chain nat REDIRECT rules that make
// the proxy transparent to the agent. Runs in the sandbox-pod's network
// namespace, so the agent (which shares the netns) inherits these rules.
//
// DNS interception (the head rules) must sit ABOVE docker's embedded-DNS
// rules. On a user-defined docker network the agent's resolver is 127.0.0.11,
// and docker installs nat OUTPUT DNAT rules for 127.0.0.11:53 at container
// create time — before sandboxd runs. Appending our DNS redirect would let
// docker's rule win and the sink would never see the default resolver's
// traffic, so we -I OUTPUT them at the head. They carry `-m mark ! --mark`:
// the agent's (unmarked) DNS is redirected to the sink, while the proxy's and
// sbxfuse's own (marked) resolver traffic is left alone to fall through to
// docker's DNAT and resolve for real.
//
// Rule order (top of OUTPUT downward):
//  1. -p udp --dport 53 -m mark ! --mark <mark> → sink  (above docker's DNAT)
//  2. -p tcp --dport 53 -m mark ! --mark <mark> → sink port (UDP-only, so the
//     connection is refused; this rule exists only to keep TCP DNS from
//     reaching docker's DNAT and resolving for real)
//     … docker's 127.0.0.11:53 DNAT (marked resolver traffic lands here) …
//  3. -m mark --mark <mark> -j RETURN — proxy/sbxfuse upstream escapes the TCP
//     redirect below; sits before it but below the narrow DNS DNAT.
//  4. -p tcp -j REDIRECT --to-ports <proxyPort> — all other TCP, loopback
//     included.
//
// Non-TCP egress is blocked outright (filter OUTPUT): TCP is the workload's
// only off-box path — it's redirected to the proxy by the nat rules above —
// so UDP, ICMP, SCTP, and raw IP have nowhere legitimate to go and are
// dropped here. This matters because the workload holds CAP_NET_RAW, which
// would otherwise let it open a raw socket and tunnel data out over ICMP
// (or any other IP protocol) around the proxy entirely. The DROP exempts
// loopback — both the lo out-interface and 127.0.0.0/8 destinations, since a
// DNS query REDIRECTed to the sink is rerouted onto lo only after this chain
// runs (so an off-loopback resolver like a kube-dns ClusterIP still shows its
// original out-interface here) — and our own marked sockets (the proxy's real
// resolver UDP/53 and its TCP upstream). Unmarked infra TCP on a real interface
// falls through and is accepted, so only non-TCP workload traffic is dropped.
//
// IPv6 egress is dropped separately (ip6tables, via dropIPv6Egress): the whole
// egress model above is IPv4-only — there is no v6 proxy or DNS-sink path — so
// any routable v6 the workload reached would bypass every control. Doing this
// with ip6tables (CAP_NET_ADMIN, same as the v4 rules) instead of disabling the
// v6 stack keeps it in sandboxd (identical under docker and k8s, no read-write
// /proc/sys needed) and leaves loopback ::1 intact.
//
// Requires CAP_NET_ADMIN; the sandbox-pod runs with NET_ADMIN for now.
func (c *container) RedirectEgress(ctx context.Context, proxyPort, dnsPort, mark int) error {
	// Packed sandbox: it runs in its own netns, so egress can't be caught by
	// OUTPUT rules in the pod netns. Instead bring up its netns/veth on the pod
	// bridge and REDIRECT bridge-forwarded traffic to sbxproxy in the host netns
	// (where conntrack + SO_ORIGINAL_DST work, preserving its source IP).
	if c.guestIP != "" {
		// Prewarmed: the pod's prewarm pool already wired this octet's
		// netns/veth/iptables, so there is nothing to do on the claim path.
		if c.prealloc {
			return nil
		}
		return c.setupPackedNet(ctx, proxyPort, dnsPort)
	}
	markHex := fmt.Sprintf("0x%x", mark)
	rules := [][]string{
		// Head: DNS → sink, above docker's embedded-DNS DNAT, skipping our own
		// marked resolver sockets. Inserted at positions 1 and 2 so they keep
		// this relative order at the top of the chain.
		{"-t", "nat", "-I", "OUTPUT", "1", "-p", "udp", "--dport", "53", "-m", "mark", "!", "--mark", markHex, "-j", "REDIRECT", "--to-ports", strconv.Itoa(dnsPort)},
		{"-t", "nat", "-I", "OUTPUT", "2", "-p", "tcp", "--dport", "53", "-m", "mark", "!", "--mark", markHex, "-j", "REDIRECT", "--to-ports", strconv.Itoa(dnsPort)},
		// Tail: exempt marked upstream traffic, then redirect all other TCP to
		// the proxy. These can append below docker's rules — docker's only nat
		// entries are the narrow 127.0.0.11:53 DNATs handled above.
		{"-t", "nat", "-A", "OUTPUT", "-m", "mark", "--mark", markHex, "-j", "RETURN"},
		{"-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-j", "REDIRECT", "--to-ports", strconv.Itoa(proxyPort)},
		// Block all non-TCP workload egress: TCP reaches the proxy via the nat
		// redirect, everything else (UDP, ICMP, SCTP, raw IP) has no off-box
		// path. Loopback (DNS to the local sink, the redirected workload TCP)
		// and our own marked sockets (proxy/sbxfuse upstream) are exempt;
		// non-TCP that matches neither is dropped. Unmarked infra TCP on a real
		// interface falls through past the drop and is accepted.
		{"-t", "filter", "-A", "OUTPUT", "-o", "lo", "-j", "RETURN"},
		{"-t", "filter", "-A", "OUTPUT", "-m", "mark", "--mark", markHex, "-j", "RETURN"},
		// Exempt loopback *destinations*, not just the lo out-interface. A DNS
		// query REDIRECTed to the sink has its destination rewritten to
		// 127.0.0.1 in nat OUTPUT, but the reroute onto lo happens only AFTER
		// filter OUTPUT — so here the packet still carries its original
		// out-interface. When the resolver is off-loopback (a Kubernetes
		// kube-dns ClusterIP, unlike docker's loopback 127.0.0.11) `-o lo`
		// misses and the redirected DNS would hit the DROP below. Matching the
		// post-REDIRECT loopback destination catches it; loopback traffic can't
		// leave the box, so this doesn't widen egress.
		{"-t", "filter", "-A", "OUTPUT", "-d", "127.0.0.0/8", "-j", "RETURN"},
		{"-t", "filter", "-A", "OUTPUT", "!", "-p", "tcp", "-j", "DROP"},
	}
	for _, args := range rules {
		out, err := exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("iptables %v: %w (%s)", args, err, bytes.TrimSpace(out))
		}
	}
	// Drop the workload's IPv6 egress. The workload shares the pod netns, so a
	// filter OUTPUT drop on NEW v6 connections covers it; loopback (::1),
	// established replies (inbound v6 to the API), and our own marked sockets
	// are exempt so only workload-initiated off-box v6 is blocked.
	return dropIPv6Egress(ctx, [][]string{
		{"-A", "OUTPUT", "-o", "lo", "-j", "RETURN"},
		{"-A", "OUTPUT", "-m", "conntrack", "--ctstate", "ESTABLISHED", "-j", "RETURN"},
		{"-A", "OUTPUT", "-m", "mark", "--mark", markHex, "-j", "RETURN"},
		{"-A", "OUTPUT", "-j", "DROP"},
	})
}

func (c *container) CgroupPath() string { return c.cgroupPath }

// FlushAgent is a no-op for the container backend: the agent's overlayfs upper
// layer is a host directory in the shared host page cache, so snapshot capture
// already reads a consistent view without an explicit sync.
func (c *container) FlushAgent(ctx context.Context) error { return nil }

func (c *container) RestoreSnapshot(src string) error {
	return snapshot.Restore(src, c.overlay.Upper, c.snapshotMounts())
}

func (c *container) CaptureSnapshot(dst string, include []string) error {
	return snapshot.Capture(dst, c.overlay.Upper, c.snapshotMounts(), include)
}

func (c *container) snapshotMounts() []snapshot.MountSource {
	out := make([]snapshot.MountSource, 0, len(c.localMounts))
	for _, m := range c.localMounts {
		out = append(out, snapshot.MountSource{ContainerPath: m.ContainerPath, HostDir: m.HostDir})
	}
	return out
}

func (c *container) Files() FileBridge {
	return containerFiles{upperDir: c.overlay.Upper, wsMountRoot: c.wsMountRoot, wsBackendRoot: c.wsBackendRoot}
}

func (c *container) LaunchAgent(cfg AgentConfig) (string, []string, error) {
	// Create the readiness fifo and hold its read end open *before* runc
	// starts, so the poststart hook's O_WRONLY open returns immediately and
	// its byte is buffered even if WaitReady hasn't read yet.
	if err := runc.MakeFifo(c.readyFifoPath); err != nil {
		return "", nil, fmt.Errorf("create ready fifo: %w", err)
	}
	f, err := os.OpenFile(c.readyFifoPath, os.O_RDWR, 0)
	if err != nil {
		return "", nil, fmt.Errorf("open ready fifo: %w", err)
	}
	c.readyFifo = f

	if err := runc.WriteConfig(runc.BundleParams{
		BundleDir:   c.bundleDir,
		RootPath:    "merged",
		ImageConfig: cfg.ImageConfig,
		ExtraEnv:    cfg.Env,
		Hostname:    "agent",
		Mounts:      cfg.Mounts,
		CgroupsPath: c.cgroupPath,
		VcpuCount:   c.vcpuCount,
		MemoryMiB:   c.memSizeMib,
		Terminal:    cfg.TTY,
		ReadyFifo:   c.readyFifoPath,
		NetnsPath:   c.netnsPath(),
	}); err != nil {
		c.readyFifo.Close()
		c.readyFifo = nil
		return "", nil, fmt.Errorf("write bundle config: %w", err)
	}
	// The workspaces recorded so far are in the bundle (cfg.Mounts), so runc
	// makes them visible to the agent itself — mark them bound so a later
	// MountWorkspacesLive only injects ones added after launch.
	c.mu.Lock()
	if c.bound == nil {
		c.bound = map[string]bool{}
	}
	for _, m := range c.workspaces {
		c.bound[m.host] = true
	}
	c.mu.Unlock()
	return "runc", []string{"run", "-b", c.bundleDir, c.containerID}, nil
}

// WaitReady blocks until the container's poststart hook signals that the
// entrypoint is running (a byte on the ready fifo) or ctx is cancelled.
func (c *container) WaitReady(ctx context.Context) error {
	if c.readyFifo == nil {
		return fmt.Errorf("ready fifo not initialized; LaunchAgent must run first")
	}
	defer c.readyFifo.Close()

	done := make(chan error, 1)
	go func() {
		_, err := c.readyFifo.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case <-ctx.Done():
		c.readyFifo.Close() // unblock the Read above
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return fmt.Errorf("read ready fifo: %w", err)
		}
		return nil
	}
}

// PrewarmSnapshot brings up the image entrypoint container off the claim path
// and keeps it running, so a claim adopts a warm, already-running workload (e.g.
// a resident browser host) instead of cold-launching. It writes the bundle via
// LaunchAgent — using the image entrypoint and the static /etc/hosts +
// /etc/resolv.conf binds (no workspaces; none are known yet) — starts `runc run`
// in the background, and waits for the poststart fifo (the entrypoint is
// running). The prewarm guest's /run/hiver/prewarm-ready is unused here. On any
// failure it tears down and returns nil so the claim falls back to cold launch
// (hasPrewarm stays false).
func (c *container) PrewarmSnapshot(ctx context.Context, imgCfg *runc.ImageConfig) error {
	if len(imgCfg.Entrypoint) == 0 && len(imgCfg.Cmd) == 0 {
		return nil // nothing config-independent to warm; cold launch on claim
	}
	bin, args, err := c.LaunchAgent(AgentConfig{
		ImageConfig: imgCfg,
		Env:         map[string]string{"NODE_EXTRA_CA_CERTS": NodeCACertPath},
		Mounts: []runc.BindMount{
			{Source: "/etc/hosts", Destination: "/etc/hosts", Options: []string{"ro"}},
			{Source: "/etc/resolv.conf", Destination: "/etc/resolv.conf", Options: []string{"ro"}},
		},
		Prewarm: true,
	})
	if err != nil {
		log.Printf("isolation: container prewarm bundle: %v (cold launch on claim)", err)
		return nil
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("isolation: container prewarm start: %v (cold launch on claim)", err)
		return nil
	}
	c.prewarmCmd = cmd
	// WaitReady reads the poststart fifo: the entrypoint is exec'd and running.
	if err := c.WaitReady(ctx); err != nil {
		log.Printf("isolation: container prewarm wait: %v (cold launch on claim)", err)
		_ = c.StopAgent(context.Background())
		c.prewarmCmd = nil
		return nil
	}
	c.hasPrewarm = true
	return nil
}

// HasPrewarmSnapshot reports whether PrewarmSnapshot brought the entrypoint
// container up, so the claim adopts it via the resume path.
func (c *container) HasPrewarmSnapshot() bool { return c.hasPrewarm }

// ResumeAgent returns an empty command: the prewarm container is already
// running, so the caller starts no child for the container backend.
func (c *container) ResumeAgent() (string, []string, error) { return "", nil, nil }

// ResumeReady is a no-op: the prewarm container is already serving.
func (c *container) ResumeReady(ctx context.Context) error { return nil }

// StopAgent destroys the agent container on the teardown path, for both launch
// shapes: a cold `runc run` (started via startChild) and a prewarmed container
// (started in the background by PrewarmSnapshot).
//
// Killing the launching process alone leaks the workload: `runc run` (and the
// prewarm `exec.Cmd`) is only the supervisor in sandboxd's PID namespace, with
// no kernel link into the container's own PID namespace. SIGKILLing it leaves
// the container's init (PID 1 of that namespace) alive, so its whole process
// tree survives and is reparented to sandboxd (PID 1). In a pack pod, where
// sandboxd outlives each sandbox, those orphans accumulate. Tearing the
// container down by id — `runc kill --all` then a force delete — is the only
// reliable stop, so do it unconditionally (the id is valid however we launched).
func (c *container) StopAgent(ctx context.Context) error {
	_ = exec.Command("runc", "kill", "--all", c.containerID, "SIGKILL").Run()
	_ = exec.Command("runc", "delete", "--force", c.containerID).Run()
	// Reap the backgrounded `runc run` from the prewarm path, if any. The cold
	// path's supervisor is owned by the caller (it Waits on agentCmd).
	if c.prewarmCmd != nil {
		_ = c.prewarmCmd.Process.Kill()
		_ = c.prewarmCmd.Wait()
		c.prewarmCmd = nil
	}
	return nil
}

// ApplyResumeState injects any workspace recorded since launch into the running
// agent container's mount namespace, so a mount added by a runtime PUT
// /v1/config — or by the first config on the prewarm resume path, where the
// container launched with no workspaces — becomes visible to the workload
// (launch-time ones are already bound by runc). Idempotent: each is bound at most
// once. The injection runs in a re-exec'd helper because it leaves a thread in
// the container's mount ns. The env argument is ignored: the entrypoint already
// launched (cold via runc, or warm at prewarm), so late env can't reach it;
// exec sessions get env per-call.
func (c *container) ApplyResumeState(ctx context.Context, _ []string) error {
	c.mu.Lock()
	if c.bound == nil {
		c.bound = map[string]bool{}
	}
	var todo []wsMount
	for _, m := range c.workspaces {
		if !c.bound[m.host] {
			todo = append(todo, m)
		}
	}
	c.mu.Unlock()
	if len(todo) == 0 {
		return nil
	}

	pid, err := c.agentPID()
	if err != nil {
		return err
	}
	for _, m := range todo {
		if err := bindWorkspaceIntoContainer(pid, m.host, m.guest); err != nil {
			return fmt.Errorf("inject workspace %s: %w", m.guest, err)
		}
		c.mu.Lock()
		c.bound[m.host] = true
		c.mu.Unlock()
		log.Printf("isolation: bound workspace %s into running container (pid %d)", m.guest, pid)
	}
	return nil
}

// agentPID returns the host PID of the running agent container's init process,
// needed to enter its mount namespace.
func (c *container) agentPID() (int, error) {
	out, err := exec.Command("runc", "state", c.containerID).Output()
	if err != nil {
		return 0, fmt.Errorf("runc state %s: %w", c.containerID, err)
	}
	var st struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return 0, fmt.Errorf("parse runc state: %w", err)
	}
	if st.Pid <= 0 {
		return 0, fmt.Errorf("runc state %s: no pid", c.containerID)
	}
	return st.Pid, nil
}

func (c *container) ExecCmd(ctx context.Context, cfg ExecConfig) (*exec.Cmd, func(), error) {
	pidPath, err := newExecPIDFile()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, "runc", c.execArgs(cfg, pidPath)...)
	cleanup := func() {
		// On client abort the in-container process may still be running
		// and orphaned; kill the whole tree. On normal exit it has
		// already gone and its PID may have been recycled, so only do
		// this when the context was cancelled.
		if ctx.Err() != nil {
			killExecTree(pidPath)
		}
		os.Remove(pidPath)
	}
	return cmd, cleanup, nil
}

// execArgs constructs the argument slice for `runc exec`.
//
// When tty is set, --tty puts runc in interactive terminal mode (it
// proxies the container pty through its own stdio, which the caller
// supplies as a pty slave).
//
// env entries are passed as `--env KEY=VALUE` flags. runc seeds the exec
// process with the container's configured environment and merges these on
// top, so callers that omit env inherit the sandbox config environment.
//
// --pid-file makes runc write the host-namespace PID of the spawned
// process so the cleanup func can kill the whole tree on teardown (SIGKILL
// of the runc process alone does not reliably reap the in-container work).
func (c *container) execArgs(cfg ExecConfig, pidFile string) []string {
	args := []string{"exec"}
	if cfg.TTY {
		args = append(args, "--tty")
	}
	if cfg.Cwd != nil && *cfg.Cwd != "" {
		args = append(args, "--cwd", *cfg.Cwd)
	}
	if pidFile != "" {
		args = append(args, "--pid-file", pidFile)
	}
	if cfg.Env != nil {
		// Sort keys so the flag order is deterministic.
		keys := make([]string, 0, len(*cfg.Env))
		for k := range *cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--env", k+"="+(*cfg.Env)[k])
		}
	}
	args = append(args, c.containerID, "sh", "-c", cfg.Command)
	return args
}

// newExecPIDFile creates an empty temp file for `runc exec --pid-file`.
// runc overwrites it with the spawned process's PID.
func newExecPIDFile() (string, error) {
	f, err := os.CreateTemp("", "hive-exec-*.pid")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	return name, nil
}

// killExecTree reads the PID runc wrote to pidPath and SIGKILLs that
// process together with every descendant. Killing the runc process does
// not reliably reap the in-container workload (runc sets no parent-death
// signal for exec'd processes), so we kill the tree explicitly.
func killExecTree(pidPath string) {
	pid, ok := readExecPID(pidPath)
	if !ok {
		return
	}
	killProcessTree(pid)
}

// readExecPID reads and parses the PID runc wrote to pidPath. runc writes
// the file right after spawning the process, so on a very early abort it
// may not exist yet; retry briefly to cover that window.
func readExecPID(pidPath string) (int, bool) {
	for i := 0; i < 10; i++ {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
				return pid, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, false
}

// killProcessTree SIGKILLs rootPID and all of its descendants. It
// snapshots the parent→child relationships from /proc first and then
// signals every member of the subtree, so descendants survive being
// re-parented to the container's init (which a naive live parent-walk
// would lose) and are still killed. PIDs are interpreted in sandboxd's PID
// namespace, where runc reports them and /proc lists in-container procs.
func killProcessTree(rootPID int) {
	if rootPID <= 1 {
		return
	}
	children := map[int][]int{}
	if entries, err := os.ReadDir("/proc"); err == nil {
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			if ppid, ok := readPPID(pid); ok {
				children[ppid] = append(children[ppid], pid)
			}
		}
	}
	victims := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for i := 0; i < len(victims); i++ {
		for _, ch := range children[victims[i]] {
			if !seen[ch] {
				seen[ch] = true
				victims = append(victims, ch)
			}
		}
	}
	for _, pid := range victims {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// readPPID returns the parent PID from /proc/<pid>/stat.
func readPPID(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parsePPIDStat(string(data))
}

// parsePPIDStat extracts the parent PID (the 4th field) from the contents
// of /proc/<pid>/stat. The comm field (2nd) is wrapped in parentheses and
// may itself contain spaces and parentheses, so the remaining
// space-separated fields are parsed after the final ')'.
func parsePPIDStat(s string) (int, bool) {
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+2 > len(s) {
		return 0, false
	}
	fields := strings.Fields(s[rparen+2:]) // state, ppid, ...
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// containerFiles serves the management file API from the host: paths under a
// configured FUSE mount resolve to that mount's backend dir; everything else
// falls back to the overlay upper layer. Both bypass sbxfuse ACLs — the file
// API is a higher-privilege control surface than the workload.
// workspaceSlug mirrors spec.FS.Slug: a filename-safe id for a mount path, used
// to locate a packed sandbox's per-key host dir (/run/sandboxd/<key>/.../<slug>).
func workspaceSlug(mount string) string {
	s := strings.ReplaceAll(strings.Trim(mount, "/"), "/", "-")
	if s == "" {
		return "root"
	}
	return s
}

type containerFiles struct {
	upperDir string
	// wsMountRoot/wsBackendRoot, when set (packed sandbox), relocate workspace
	// paths to the per-key host dirs instead of the default <mount>-backend.
	wsMountRoot   string
	wsBackendRoot string
}

func (f containerFiles) List(agentPath string, mounts []MountRoute) ([]FileEntry, error) {
	entries, err := os.ReadDir(f.hostPath(agentPath, mounts))
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		var size int64
		if !e.IsDir() {
			size = info.Size()
		}
		out = append(out, FileEntry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}
	return out, nil
}

func (f containerFiles) Open(agentPath string, mounts []MountRoute) (io.ReadCloser, int64, error) {
	host := f.hostPath(agentPath, mounts)
	info, err := os.Stat(host)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("not a regular file")
	}
	fh, err := os.Open(host)
	if err != nil {
		return nil, 0, err
	}
	return fh, info.Size(), nil
}

func (f containerFiles) Stat(agentPath string, mounts []MountRoute) (FileEntry, error) {
	host := f.hostPath(agentPath, mounts)
	info, err := os.Stat(host)
	if err != nil {
		return FileEntry{}, err
	}
	var size int64
	if !info.IsDir() {
		size = info.Size()
	}
	return FileEntry{Name: filepath.Base(host), IsDir: info.IsDir(), Size: size}, nil
}

func (f containerFiles) Delete(agentPath string, mounts []MountRoute) error {
	return os.Remove(f.hostPath(agentPath, mounts))
}

func (f containerFiles) Save(agentDir, name string, mounts []MountRoute, r io.Reader) (int64, error) {
	hostDir := f.hostPath(agentDir, mounts)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return 0, err
	}
	target := filepath.Join(hostDir, name)
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return 0, closeErr
	}
	return n, nil
}

// hostPath maps an agent-visible absolute path to its host location by
// longest-prefix match against the configured mounts:
//
//   - local-backend mount → that mount's "-backend" dir (the source of truth;
//     reading it bypasses sbxfuse ACLs, which the file API intentionally does).
//   - remote-backed mount → the FUSE mount point itself. The "-backend" dir is
//     only a write buffer the oplog evicts after flushing to the remote, so it
//     would miss already-flushed files; the FUSE mount serves the merged
//     remote+local view (and routes writes back through the oplog).
//   - no match → the overlay upper layer.
func (f containerFiles) hostPath(agentPath string, mounts []MountRoute) string {
	cleaned := filepath.Clean(agentPath)
	var matched MountRoute
	for _, m := range mounts {
		if cleaned == m.Mount || strings.HasPrefix(cleaned, strings.TrimRight(m.Mount, "/")+"/") {
			if len(m.Mount) > len(matched.Mount) {
				matched = m
			}
		}
	}
	if matched.Mount != "" {
		rel := strings.TrimPrefix(cleaned, matched.Mount)
		// Packed sandbox: workspaces live under the per-key host roots, not the
		// default <mount>-backend / <mount> layout.
		if f.wsBackendRoot != "" {
			root := f.wsBackendRoot
			if matched.Remote {
				root = f.wsMountRoot
			}
			return filepath.Join(root, workspaceSlug(matched.Mount), rel)
		}
		if matched.Remote {
			return filepath.Join(matched.Mount, rel)
		}
		return filepath.Join(matched.Mount+spec.BackendSuffix, rel)
	}
	return filepath.Join(f.upperDir, cleaned)
}
