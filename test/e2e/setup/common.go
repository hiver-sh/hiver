package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blasten/hive/internal/spec"
	"sigs.k8s.io/yaml"
)

const (
	moduleRoot          = "../.."
	sandboxRuntimeImage = "hive-sandbox-runtime" // :latest — sandboxd + sidecars + runc
	// Pinned ports for the host-side upstream HTTP servers. They have
	// to be fixed because the spec.yaml fixture references them
	// literally (no template substitution). Picked high enough to
	// avoid common dev-tool collisions.
	upstreamAllowedPort = 17080
	upstreamDeniedPort  = 17081
	// sandboxd's API + SSE port inside the pod, published to the host
	// so the e2e harness can subscribe to /v1/events.
	apiServerPort = 8080
)

// RunFixtureE2E orchestrates a single end-to-end run for the named
// fixture under test/e2e/fixtures/<fixtureName>/. The fixture must
// contain Dockerfile + spec.yaml + expectations.yaml; everything else
// (sandbox-runtime image, host upstreams, audit assertions) is shared.
// RunFixtureE2E orchestrates a full E2E run for the named fixture.
//
// Optional mutators receive the spec parsed from the fixture (before
// validation) and can fill in fields supplied at test time — e.g.
// gdrive auth tokens that come from env vars and can't be checked in.
// Mutators run in order; after all of them the spec is validated and
// re-rendered to a tmpfile that the pod actually mounts.
func RunFixtureE2E(t *testing.T, fixtureName string, mutators ...func(*spec.Spec)) {
	runFixture(t, fixtureName, fixtureRun{Mutators: mutators})
}

// FixtureHook is the signature for callbacks that run while the pod
// is alive — after the agent's DONE marker (and the ingress/exec
// probes) but before sandboxd is SIGTERM'd. baseURL is the host-side
// URL of sandboxd's API server inside the pod (e.g.
// http://127.0.0.1:8080).
type FixtureHook func(t *testing.T, baseURL string)

// RunFixtureE2EHook is like RunFixtureE2E but invokes `hook` once,
// inside the pod's live window, before shutdown. Use it for assertions
// that need to query the running sandbox (e.g. `/v1/events` resume).
// All the standard fixture-level assertions (substrings, egress, fs)
// still run.
func RunFixtureE2EHook(t *testing.T, fixtureName string, hook FixtureHook, mutators ...func(*spec.Spec)) {
	runFixture(t, fixtureName, fixtureRun{Mutators: mutators, DuringLifetime: hook})
}

// fixtureRun is the internal config struct shared by the public
// entry points.
type fixtureRun struct {
	Mutators       []func(*spec.Spec)
	DuringLifetime FixtureHook
}

func runFixture(t *testing.T, fixtureName string, cfg fixtureRun) {
	t.Helper()
	RequireDocker(t)
	mutators := cfg.Mutators

	fixtureDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures", fixtureName))
	if err != nil {
		t.Fatalf("abs fixture dir: %v", err)
	}
	specPath := filepath.Join(fixtureDir, "spec.yaml")
	// Parse without validating so mutators can fix up missing fields
	// (e.g. fill in auth tokens) before Validate runs.
	sp, err := spec.Parse(specPath)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	for _, m := range mutators {
		m(sp)
	}
	if err := sp.Validate(); err != nil {
		t.Fatalf("validate spec: %v", err)
	}
	if len(mutators) > 0 {
		// Re-render the mutated spec to a tmpfile so the pod sees the
		// runtime-supplied fields.
		rendered, err := yaml.Marshal(sp)
		if err != nil {
			t.Fatalf("re-render spec: %v", err)
		}
		specPath = filepath.Join(t.TempDir(), "spec.yaml")
		if err := os.WriteFile(specPath, rendered, 0o644); err != nil {
			t.Fatalf("write spec tmpfile: %v", err)
		}
		t.Logf("rendered spec at %s:\n%s", specPath, redactSecrets(string(rendered)))
	}
	// `image:` may name either a directory containing a Dockerfile
	// (e.g. `.`) or the Dockerfile itself (e.g.
	// `../../../../docker/mcpserver.Dockerfile`). Resolve both shapes
	// to the actual Dockerfile path before building.
	imagePath := sp.Image
	if !filepath.IsAbs(imagePath) {
		imagePath = filepath.Join(fixtureDir, imagePath)
	}
	dockerfile := imagePath
	if info, err := os.Stat(imagePath); err != nil {
		t.Fatalf("stat image %q: %v", imagePath, err)
	} else if info.IsDir() {
		dockerfile = filepath.Join(imagePath, "Dockerfile")
	}
	// When the Dockerfile lives outside the fixture directory (e.g.
	// `../../../../docker/mcpserver.Dockerfile` to reuse an image from
	// the repo), it likely needs the module root as its build context
	// so it can see go.mod and the source tree. For self-contained
	// fixtures (image: .) the fixture dir IS the build context.
	buildContext := filepath.Dir(dockerfile)
	if rel, err := filepath.Rel(fixtureDir, dockerfile); err == nil && strings.HasPrefix(rel, "..") {
		buildContext, err = filepath.Abs(moduleRoot)
		if err != nil {
			t.Fatalf("abs module root: %v", err)
		}
	}

	agentImage := "sandbox-" + fixtureName + ":e2e"
	bundleImage := "sandbox-bundle-" + fixtureName + ":e2e"
	BuildImages(t, dockerfile, buildContext, agentImage)
	BuildSandboxBundle(t, agentImage, bundleImage)

	// Start the two host-side upstreams on the ports the fixture spec
	// pins. One is allowlisted (host-aliased to "upstream-allowed"),
	// the other isn't. Cheap to start even for fixtures that don't
	// exercise HTTP egress — left running so we don't fork the helper.
	stopUpstream := startUpstreams(t)
	defer stopUpstream()

	pod := runSandboxPod(t, bundleImage, specPath, cfg.DuringLifetime)

	// (1) Substring assertions against the pod's combined stdout/stderr.
	// Expected lines live in the fixture's expectations.yaml so the
	// fixture is self-describing (Dockerfile + agent.py + spec.yaml +
	// expectations.yaml all in one directory). Most entries assert
	// agent-printed "[sandbox:out] …" lines, but any substring of the
	// container output is fair game (sandboxd lifecycle, audit-tail
	// "agent op | …", etc.).
	expectationsPath := filepath.Join(fixtureDir, "expectations.yaml")
	for _, want := range loadExpectations(t, expectationsPath) {
		if !strings.Contains(pod.output, want.Substring) {
			t.Errorf("missing %s: no substring %q in pod output", want.Desc, want.Substring)
		}
	}

	// (2) Egress: assert at least one allowed + one denied egress.request
	// SandboxEvent — but only when the fixture actually drives HTTP
	// traffic. Zero egress events means "this fixture doesn't use the
	// proxy" (e.g. agent-gdrive-fs only exercises the FS).
	egressRequests := filterEvents(pod.events, "egress.request")
	if len(egressRequests) > 0 {
		accesses := countByField(egressRequests, "access")
		if accesses["allowed"] < 1 {
			t.Errorf("egress.request: expected ≥1 allowed; got %v", accesses)
		}
		if accesses["denied"] < 1 {
			t.Errorf("egress.request: expected ≥1 denied; got %v", accesses)
		}
		// Every allowed HTTP-shaped request should pair with an
		// egress.response carrying the matching request_id. CONNECT /
		// raw-forward TLS don't get a response, so this is a "best
		// effort" check — only fail when we have allows but no responses.
		responses := filterEvents(pod.events, "egress.response")
		if len(responses) == 0 && accesses["allowed"] > 0 {
			t.Errorf("egress.response: expected ≥1 paired response for %d allowed requests", accesses["allowed"])
		}
	}

	// (3) FS: assert ≥1 write-allow fs.request; deny on /secret/... only
	// checked when the fixture has a /secret rule. Every allowed
	// fs.request reached the backend, so an fs.response must follow.
	fsRequests := filterEvents(pod.events, "fs.request")
	fsResponses := filterEvents(pod.events, "fs.response")
	allowedFs := 0
	for _, e := range fsRequests {
		if a, _ := e["access"].(string); a == "allowed" {
			allowedFs++
		}
	}
	if allowedFs > 0 && len(fsResponses) == 0 {
		t.Errorf("fs.response: expected ≥1 paired response for %d allowed fs.request", allowedFs)
	}
	var sawWriteAllow, sawSecretDeny bool
	for _, e := range fsRequests {
		op, _ := e["operation"].(string)
		access, _ := e["access"].(string)
		path, _ := e["path"].(string)
		if op == "write" && access == "allowed" {
			sawWriteAllow = true
		}
		if access == "denied" && strings.HasPrefix(path, "/workspace/secret") {
			sawSecretDeny = true
		}
	}
	if !sawWriteAllow {
		t.Errorf("fs.request: no write/allowed event; got %d fs.request total", len(fsRequests))
	}
	hasSecretRule := false
	for i := range sp.FS {
		for _, r := range sp.FS[i].ACLs {
			if strings.Contains(r.Path, "/secret") {
				hasSecretRule = true
				break
			}
		}
	}
	if hasSecretRule && !sawSecretDeny {
		t.Error("fs.request: no denied event on /workspace/secret/...")
	}

	// (4) sandboxd's own lifecycle log lines.
	wantSubstrings := []string{
		"sandboxd: work dir = ",
		"[sbxproxy:err]",
		"sbxproxy listening (transparent)",
		"sandboxd: iptables OUTPUT nat redirect",
		"[sbxfuse:workspace:err]",
		"sbxfuse: mounted",
		"sandboxd: agent image unpacked to",
		"sandboxd: agent op |",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(pod.output, want) {
			t.Errorf("sandboxd logs missing expected substring %q", want)
		}
	}

	if t.Failed() {
		t.Logf("\n----- container output -----\n%s\n", pod.output)
		t.Logf("\n----- SSE events (%d) -----\n%s\n", len(pod.events), SummarizeEvents(pod.events))
	}
}

// filterEvents returns events of one SandboxEvent type. The `type`
// field is set on every variant by the gen.SandboxEvent.From*Event
// helpers.
func filterEvents(events []map[string]any, t string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if got, _ := e["type"].(string); got == t {
			out = append(out, e)
		}
	}
	return out
}

// SummarizeEvents renders the collected stream as one line per event.
// Suitable for `t.Log` / failure logs — full JSON would be unreadable
// for runs with hundreds of events.
func SummarizeEvents(events []map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		id, _ := e["id"].(float64)
		typ, _ := e["type"].(string)
		fmt.Fprintf(&b, "  #%d %s", int(id), typ)
		for _, k := range []string{"access", "host", "path", "method", "operation", "status", "duration_ms", "request_id", "backend"} {
			if v, ok := e[k]; ok {
				fmt.Fprintf(&b, " %s=%v", k, v)
			}
		}
		// stdio carries one of stdout/stderr per event; quote the chunk
		// so trailing whitespace is visible and a runaway newline can't
		// break the one-line-per-event format.
		for _, k := range []string{"stdout", "stderr"} {
			if v, ok := e[k].(string); ok {
				fmt.Fprintf(&b, " %s=%q", k, v)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// redactSecrets masks token-like values in YAML so the rendered-spec
// debug log doesn't leak credentials. We only need the structure +
// presence/absence of fields when debugging the gdrive path.
var secretFieldRE = regexp.MustCompile(`(?m)^(\s*(?:gdrive_access_token|gdrive_refresh_token|gdrive_client_secret|gdrive_service_account_json)\s*:\s*).+$`)

func redactSecrets(s string) string {
	return secretFieldRE.ReplaceAllString(s, `${1}<redacted>`)
}

// RequireDocker skips the test if no Docker daemon is reachable.
func RequireDocker(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Skipf("Docker not available: %v", err)
	}
}

// BuildImages builds the two independent images this test needs:
// sandbox-runtime (the pod, always at the module root) and the agent
// image. dockerfile is the absolute path to the agent image's
// Dockerfile; buildContext is the docker build context — usually the
// Dockerfile's directory, but moduleRoot for fixtures that reuse a
// Dockerfile from elsewhere in the repo (see runFixtureE2E for the rule).
func BuildImages(t *testing.T, dockerfile, buildContext, agentImage string) {
	t.Helper()
	build := func(tag, contextDir string, extraArgs ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		args := append([]string{"build", "-t", tag}, extraArgs...)
		args = append(args, contextDir)
		cmd := exec.CommandContext(ctx, "docker", args...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker build %s: %v\n%s", tag, err, out.String())
		}
	}
	build(sandboxRuntimeImage, moduleRoot, "-f", filepath.Join(moduleRoot, "docker/sandbox-runtime.Dockerfile"))
	build(agentImage, buildContext, "-f", dockerfile)
}

// BuildSandboxBundle calls bundle-images.sh to package agentImage
// into a sandbox-bundle image tagged bundleTag. The bundle has the agent
// rootfs pre-extracted at /mnt so sandboxd can skip the runtime unpack.
func BuildSandboxBundle(t *testing.T, agentImage, bundleTag string) {
	t.Helper()
	absRoot, err := filepath.Abs(moduleRoot)
	if err != nil {
		t.Fatalf("abs module root: %v", err)
	}
	scriptPath := filepath.Join(absRoot, "scripts/bundle-images.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", scriptPath, agentImage, bundleTag)
	cmd.Dir = absRoot
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("bundle-images.sh %s: %v\n%s", agentImage, err, out.String())
	}
}

// startUpstreams spins up two HTTP servers on the host on the ports the
// fixture's spec.yaml pins. Both bind to all interfaces (so Docker can
// reach them via the host-gateway alias). They're aliased inside the
// container as:
//
//	upstream-allowed → host  (matches an allow rule in spec.egress)
//	upstream-denied  → host  (does NOT match; proxy 403s)
func startUpstreams(t *testing.T) (stop func()) {
	t.Helper()
	mkServer := func(port int, handler http.HandlerFunc) *http.Server {
		l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			t.Fatalf("listen :%d: %v (port collision? the spec.yaml pins this port)", port, err)
		}
		srv := &http.Server{Handler: handler}
		go func() { _ = srv.Serve(l) }()
		return srv
	}
	allowedSrv := mkServer(upstreamAllowedPort, func(w http.ResponseWriter, r *http.Request) {
		// Header override: spec.yaml has the rule inject
		// "X-Sandbox-Agent: agent-python". If the proxy isn't applying
		// the override, the agent's request reaches the upstream
		// without it — which would be a silent escape.
		if got := r.Header.Get("X-Sandbox-Agent"); got != "agent-python" {
			t.Errorf("upstream did not see injected header: got %q, want %q", got, "agent-python")
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("from-allowed"))
	})
	deniedSrv := mkServer(upstreamDeniedPort, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("denied upstream was reached — proxy let it through")
		w.WriteHeader(500)
	})
	return func() {
		_ = allowedSrv.Close()
		_ = deniedSrv.Close()
	}
}

// runSandboxPod runs the sandbox-runtime container with the agent image
// tarball + spec bind-mounted in, and returns its combined stdout+stderr.
//
// The agent is intentionally silent and blocks forever after running its
// probes (so the pod stays up for docker-exec inspection during local
// debugging). The orchestrator's view of the agent's behavior comes from
// sandboxd's "agent op | …" lines, which it emits as it tails the proxy
// and FUSE audit streams. Once we've seen the agent's DONE sentinel,
// we send SIGTERM and watch the docker-run subprocess's exit channel
// directly — no fixed shutdown timeout. sandboxd's graceful chain
// (sbxfuse oplog drain → FUSE unmount → child reap) runs in whatever
// time it actually needs; the only upper bound is the outer test
// deadline. SIGKILL via `docker kill` skips the drain entirely, so we
// only fall back to it if SIGTERM doesn't take effect in time —
// matters for remote-backed workspaces (gdrive et al.) where pending
// uploads would otherwise be lost.
//
// Hand the pod the specific capabilities runc/iptables/FUSE need,
// and disable seccomp and AppArmor (their default filters block syscalls
// runc uses to create namespaces and pivot_root the inner container).
// /dev/fuse is the only device exposed.
// podRun is what runSandboxPod returns: the container's combined
// stdout+stderr plus every SandboxEvent the SSE harness collected
// during the container's lifetime.
type podRun struct {
	output string
	events []map[string]any
}

func runSandboxPod(t *testing.T, bundleImage, specPath string, duringLifetime FixtureHook) podRun {
	t.Helper()
	// 2 min outer deadline. The graceful-shutdown path adds ~25 s
	// (sbxfuse drain + sandboxd WaitDelay) on top of the sandbox's
	// own runtime, so the previous 90 s budget was tight.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	containerName := fmt.Sprintf("sandbox-pod-e2e-%d", time.Now().UnixNano())

	args := []string{
		"run", "-d", "--rm",
		"--name", containerName,
		// /dev/fuse is the only host device exposed; sbxfuse opens it
		// to mount the workspace. /dev/* is otherwise hidden.
		"--device", "/dev/fuse",
		// Capability set, narrower than the all-caps grant of
		// --privileged. Each entry is justified by something runc,
		// iptables, or sbxfuse actually does.

		// CLONE_NEW{PID,NS,IPC,UTS} for runc to spin up the agent's
		// namespaces; mount(2) for sbxfuse + the runc bundle's
		// proc/sys/dev/pts/shm mounts; FUSE init.
		"--cap-add", "SYS_ADMIN",
		// Netfilter writes — sandboxd installs OUTPUT-chain nat REDIRECT
		// for transparent egress and uses SO_MARK on proxy upstream
		// sockets, both of which require CAP_NET_ADMIN.
		"--cap-add", "NET_ADMIN",
		// runc populates /dev/{null,zero,full,random,urandom,tty,...}
		// inside the agent rootfs via mknod(2).
		"--cap-add", "MKNOD",
		// runc swaps the process root to the agent rootfs via
		// pivot_root / chroot during bundle setup.
		"--cap-add", "SYS_CHROOT",
		// runc drops the agent process down to the bundle's bounding
		// capability set (PR_CAP_AMBIENT, capset(2)) before exec.
		"--cap-add", "SETPCAP",
		// runc copies file capabilities (xattr security.capability)
		// into the agent rootfs — needs CAP_SETFCAP.
		"--cap-add", "SETFCAP",
		// runc switches the agent process to the spec's user.uid/gid
		// before exec.
		"--cap-add", "SETUID",
		"--cap-add", "SETGID",
		// Walking the docker-archive tar into the agent rootfs touches
		// directories owned by other UIDs (the image's user). Without
		// DAC_READ_SEARCH the bundle prep can't traverse them.
		"--cap-add", "DAC_READ_SEARCH",
		// Same prep step needs to chown extracted files to match the
		// image's owner metadata; FOWNER is also required so chmod /
		// utime succeed on files we don't own.
		"--cap-add", "FOWNER",
		"--cap-add", "CHOWN",

		// AppArmor's docker-default profile blocks several mount(2)
		// flags runc needs (notably MS_REC + bind on /proc); turn the
		// LSM off rather than ship a custom profile.
		"--security-opt", "apparmor=unconfined",
		// The default seccomp filter denies syscalls runc requires
		// (clone with new namespace flags, pivot_root, mount, keyctl,
		// unshare(CLONE_NEWUSER), etc.). A tightened custom profile
		// is the natural next step; for now keep seccomp off.
		"--security-opt", "seccomp=unconfined",
		// runc creates the agent's cgroup under /sys/fs/cgroup; that
		// tree is read-only in non-privileged containers, so bind it
		// back rw. Strictly less than --privileged because we still
		// don't grant access to /dev/* or other host bind paths.
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"--add-host", "upstream-allowed:host-gateway",
		"--add-host", "upstream-denied:host-gateway",
		// Publish sandboxd's API server so the host-side e2e harness can
		// subscribe to /v1/events while the workload runs.
		"-p", fmt.Sprintf("%d:%d", apiServerPort, apiServerPort),
		"-p", "18000:18000",
		"-v", specPath + ":/mnt/spec.yaml:ro",
		bundleImage,
		"--spec", "/mnt/spec.yaml",
	}
	// Start the pod detached. With -d, `docker run` returns once the
	// container exists; we then fetch its combined stdout+stderr via
	// `docker logs -f` (mirrors run-fixture.sh) rather than reading
	// from a host-side audit file. t.Cleanup catches the panic path —
	// in the happy path SIGTERM below stops the container and --rm
	// reaps it.
	runCmd := exec.CommandContext(ctx, "docker", args...)
	var runOut bytes.Buffer
	runCmd.Stdout, runCmd.Stderr = &runOut, &runOut
	if err := runCmd.Run(); err != nil {
		t.Fatalf("docker run -d: %v\n%s", err, runOut.String())
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "kill", containerName).Run()
	})

	var out bytes.Buffer
	idleSeen := make(chan struct{})
	w := &lineSentinel{
		out: &out,
		// agent.py prints "DONE" once it has completed every probe (just
		// before entering its sleep loop). Wait for that one line.
		marker:   []byte("[sandbox:out] DONE"),
		minCount: 1,
		fired:    idleSeen,
	}

	// `docker logs -f` replays from the start of the container's log and
	// then streams new lines as they arrive, so the lineSentinel sees
	// every byte the container produces. It exits on its own once the
	// container is removed (which --rm does after exit).
	logsCmd := exec.CommandContext(ctx, "docker", "logs", "-f", containerName)
	logsCmd.Stdout, logsCmd.Stderr = w, w
	if err := logsCmd.Start(); err != nil {
		t.Fatalf("docker logs -f: %v", err)
	}
	logsDone := make(chan struct{})
	go func() {
		_ = logsCmd.Wait()
		close(logsDone)
	}()

	// Subscribe to /v1/events while the pod is alive. The collector
	// retries until sandboxd's API server binds, then streams every
	// SandboxEvent into an in-memory slice until the connection drops
	// (container exit) or sse.stop() is called.
	sse := startSSECollector(t, fmt.Sprintf("http://127.0.0.1:%d", apiServerPort))

	// `docker wait` blocks until the container exits; its stdout is the
	// container's exit code, which we don't need — the channel signal
	// alone tells us the pod is gone.
	done := make(chan error, 1)
	go func() {
		waitCmd := exec.CommandContext(ctx, "docker", "wait", containerName)
		_, err := waitCmd.Output()
		done <- err
	}()

	select {
	case <-idleSeen:
		// Agent has emitted DONE — its ingress HTTP server has been up
		// since startup, so the host can probe it now to verify
		// host→agent traffic flows. Then give sandboxd's audit-tail
		// goroutine (which polls every 100 ms) a settle window so the
		// last verdicts and the INGRESS line have made it to stdout.
		sendIngressProbe(t)
		sendExecProbe(t)
		time.Sleep(500 * time.Millisecond)

		// Caller-supplied hook (e.g. lastEventId resume checks). Runs
		// while the pod is still serving so the broker is live; any
		// new events it triggers land in `sse` and in pod.events.
		if duringLifetime != nil {
			duringLifetime(t, fmt.Sprintf("http://127.0.0.1:%d", apiServerPort))
		}

		// SIGTERM the pod and wait for it to exit on its own. We watch
		// `done` (the docker-run subprocess's Wait) directly — that
		// channel closes the moment the container's PID 1 exits, so
		// we don't pay any fixed timeout. sandboxd's own graceful
		// shutdown (sbxfuse oplog drain, FUSE unmount, child reap)
		// runs to completion in whatever time it actually needs. If
		// something inside hangs, the outer ctx (120s) is the upper
		// bound and trips the `<-ctx.Done()` arm below.
		_ = exec.Command("docker", "kill", "-s", "TERM", containerName).Run()
		select {
		case <-done:
			// graceful exit — nothing to do
		case <-ctx.Done():
			t.Logf("graceful shutdown didn't complete before outer deadline; SIGKILL")
			_ = exec.Command("docker", "kill", containerName).Run()
			<-done
			t.Fatalf("container did not exit after SIGTERM within deadline\n%s", out.String())
		}
	case err := <-done:
		// Container exited before reaching the idle marker — that's a
		// failure (something crashed). Fall through with the partial
		// output so the assertions can produce diagnostics.
		t.Logf("container exited before agent reached idle: %v", err)
	case <-ctx.Done():
		// Hard-kill on timeout: at this point we've already waited the
		// outer deadline, graceful shutdown is moot.
		_ = exec.Command("docker", "kill", containerName).Run()
		<-done
		t.Fatalf("container timed out\n%s", out.String())
	}

	// Drain the `docker logs -f` subprocess so trailing output (anything
	// the daemon flushed between `docker wait` returning and the log
	// stream closing) is mirrored into `out` before we read it. Bound
	// the wait so a wedged daemon can't hang the test.
	select {
	case <-logsDone:
	case <-time.After(5 * time.Second):
	}
	return podRun{output: out.String(), events: sse.stop()}
}

// sendIngressProbe posts a small payload to the agent's ingress
// listener (published by `docker run -p 18000:18000`). The agent
// prints "INGRESS POST <path> <body!r>" on receipt, which the test
// then asserts via expectations.yaml. Failures are logged but
// non-fatal — fixtures without an HTTP listener simply won't have
// an INGRESS expectation and the test still passes.
func sendIngressProbe(t *testing.T) {
	t.Helper()
	body := strings.NewReader("hello-from-host")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:18000/hello", body)
	if err != nil {
		t.Logf("ingress probe: build request: %v", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("ingress probe: %v (the agent may not expose an HTTP listener; ignore for non-python fixtures)", err)
		return
	}
	resp.Body.Close()
}

// sendExecProbe POSTs a bash command to the agent's /exec endpoint and
// asserts the returned JSON ({exit_code, stdout, stderr}) matches what
// running `echo hello-from-exec; exit 7` should produce. This proves
// host→agent command execution round-trips: the host can drive the
// agent and read back its results.
func sendExecProbe(t *testing.T) {
	t.Helper()
	const command = "echo hello-from-exec; exit 7"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:18000/exec", strings.NewReader(command))
	if err != nil {
		t.Errorf("exec probe: build request: %v", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("exec probe: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("exec probe: status %d, want 200", resp.StatusCode)
		return
	}
	var result struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Errorf("exec probe: decode response: %v", err)
		return
	}
	if result.ExitCode != 7 {
		t.Errorf("exec probe: exit_code=%d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello-from-exec") {
		t.Errorf("exec probe: stdout=%q, want substring %q", result.Stdout, "hello-from-exec")
	}
}

// lineSentinel mirrors writes into out and closes fired once it has
// observed minCount lines containing marker. Used to wait for an
// expected log volume without parsing structured output.
type lineSentinel struct {
	out      *bytes.Buffer
	marker   []byte
	minCount int
	fired    chan struct{}
	once     sync.Once
	pending  []byte
	seen     int
}

func (s *lineSentinel) Write(p []byte) (int, error) {
	n, _ := s.out.Write(p)
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		line := s.pending[:i]
		s.pending = s.pending[i+1:]
		if bytes.Contains(line, s.marker) {
			s.seen++
			if s.seen >= s.minCount {
				s.once.Do(func() { close(s.fired) })
			}
		}
	}
	return n, nil
}

// Expectation is one entry from the fixture's expectations.yaml. The
// fields use json tags so sigs.k8s.io/yaml can decode via JSON.
type Expectation struct {
	Desc      string `json:"desc"`
	Substring string `json:"substring"`
}

type expectationsFile struct {
	Ops []Expectation `json:"ops"`
}

// loadExpectations reads the fixture's expectations.yaml — a list of
// {desc, substring} entries the test asserts are present in sandboxd's
// "agent op | …" stream.
func loadExpectations(t *testing.T, path string) []Expectation {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var f expectationsFile
	if err := yaml.Unmarshal(body, &f); err != nil {
		t.Fatalf("parse expectations: %v", err)
	}
	return f.Ops
}

func countByField(events []map[string]any, field string) map[string]int {
	out := map[string]int{}
	for _, e := range events {
		if v, ok := e[field].(string); ok {
			out[v]++
		}
	}
	return out
}
