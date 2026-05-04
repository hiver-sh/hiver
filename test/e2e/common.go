package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandbox-platform/agent-sandbox/internal/spec"
	"go.yaml.in/yaml/v2"
)

const (
	moduleRoot          = "../.."
	sandboxRuntimeImage = "sandbox-runtime" // :latest — sandboxd + sidecars + runc
	// Pinned ports for the host-side upstream HTTP servers. They have
	// to be fixed because the spec.yaml fixture references them
	// literally (no template substitution). Picked high enough to
	// avoid common dev-tool collisions.
	upstreamAllowedPort = 17080
	upstreamDeniedPort  = 17081
)

// runFixtureE2E orchestrates a single end-to-end run for the named
// fixture under test/e2e/fixtures/<fixtureName>/. The fixture must
// contain Dockerfile + spec.yaml + expectations.yaml; everything else
// (sandbox-runtime image, host upstreams, audit assertions) is shared.
func runFixtureE2E(t *testing.T, fixtureName string) {
	t.Helper()
	requireDocker(t)

	fixtureDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures", fixtureName))
	if err != nil {
		t.Fatalf("abs fixture dir: %v", err)
	}
	specPath := filepath.Join(fixtureDir, "spec.yaml")
	sp, err := spec.Load(specPath)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	agentDir := sp.Agent.Image
	if !filepath.IsAbs(agentDir) {
		agentDir = filepath.Join(fixtureDir, agentDir)
	}

	agentImage := "sandbox-" + fixtureName + ":e2e"
	buildImages(t, agentDir, agentImage)
	agentTar := saveAgentImage(t, agentImage)

	// Start the two host-side upstreams on the ports the fixture spec
	// pins. One is allowlisted (host-aliased to "upstream-allowed"),
	// the other isn't.
	stopUpstream := startUpstreams(t)
	defer stopUpstream()

	auditDir := t.TempDir()

	output := runSandboxPod(t, agentTar, specPath, auditDir)

	// (1) Substring assertions against the pod's combined stdout/stderr.
	// Expected lines live in the fixture's expectations.yaml so the
	// fixture is self-describing (Dockerfile + agent.py + spec.yaml +
	// expectations.yaml all in one directory). Most entries assert
	// agent-printed "[agent:out] …" lines, but any substring of the
	// container output is fair game (sandboxd lifecycle, audit-tail
	// "agent op | …", etc.).
	expectationsPath := filepath.Join(fixtureDir, "expectations.yaml")
	for _, want := range loadExpectations(t, expectationsPath) {
		if !strings.Contains(output, want.Substring) {
			t.Errorf("missing %s: no substring %q in pod output", want.Desc, want.Substring)
		}
	}

	// (2) Proxy audit log on disk — at least one allow + one deny verdict.
	proxyEvents := readJSONLines(t, filepath.Join(auditDir, "proxy.log"))
	verdicts := countByField(proxyEvents, "verdict")
	if verdicts["allow"] < 1 {
		t.Errorf("proxy audit: expected ≥1 allow; got %v", verdicts)
	}
	if verdicts["deny"] < 1 {
		t.Errorf("proxy audit: expected ≥1 deny; got %v", verdicts)
	}

	// (3) FUSE audit log on disk — write-allow somewhere + deny on /secret/...
	fuseEvents := readJSONLines(t, filepath.Join(auditDir, "fuse.log"))
	var sawWriteAllow, sawSecretDeny bool
	for _, e := range fuseEvents {
		op, _ := e["op"].(string)
		v, _ := e["verdict"].(string)
		path, _ := e["path"].(string)
		if op == "write" && v == "allow" {
			sawWriteAllow = true
		}
		if v == "deny" && strings.HasPrefix(path, "/secret") {
			sawSecretDeny = true
		}
	}
	if !sawWriteAllow {
		t.Error("FUSE audit: no write-allow verdict")
	}
	if !sawSecretDeny {
		t.Error("FUSE audit: no deny verdict on /secret/...")
	}

	// (4) sandboxd's own lifecycle log lines.
	wantSubstrings := []string{
		"sandboxd: audit dir = ",
		"[sbxproxy:err]",
		"sbxproxy listening (transparent)",
		"sandboxd: iptables OUTPUT nat redirect",
		"[sbxfuse:err]",
		"sbxfuse: mounted",
		"sandboxd: agent image unpacked to",
		"sandboxd: agent op |",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(output, want) {
			t.Errorf("sandboxd logs missing expected substring %q", want)
		}
	}

	if t.Failed() {
		t.Logf("\n----- container output -----\n%s\n", output)
	}
}

// requireDocker skips the test if no Docker daemon is reachable.
func requireDocker(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Skipf("Docker not available: %v", err)
	}
}

// buildImages builds the two independent images this test needs:
// sandbox-runtime (the pod, always at the module root) and the agent
// image at agentDir, tagged as agentImage. They're not layered — each
// is its own root.
func buildImages(t *testing.T, agentDir, agentImage string) {
	t.Helper()
	build := func(tag, contextDir string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, contextDir)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker build %s: %v\n%s", tag, err, out.String())
		}
	}
	build(sandboxRuntimeImage, moduleRoot)
	build(agentImage, agentDir)
}

// saveAgentImage runs `docker save` to produce a docker-archive tarball
// of the named agent image. sandboxd unpacks this tar into an OCI
// rootfs at container start. The path is returned so the test can
// bind-mount it.
func saveAgentImage(t *testing.T, agentImage string) string {
	t.Helper()
	tarPath := filepath.Join(t.TempDir(), "agent.tar")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "save", "-o", tarPath, agentImage)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker save %s: %v\n%s", agentImage, err, out.String())
	}
	return tarPath
}

// startUpstreams spins up two HTTP servers on the host on the ports the
// fixture's spec.yaml pins. Both bind to all interfaces (so Docker can
// reach them via the host-gateway alias). They're aliased inside the
// container as:
//
//	upstream-allowed → host  (matches the spec.egress.allow list)
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
// and FUSE audit streams. We expect 5 such lines for the standard probe
// sequence (2 proxy verdicts + 3 FUSE ops); once we've seen them, we
// `docker kill` the named container and collect the captured output.
//
// Hand the pod the specific capabilities runc/iptables/FUSE need,
// and disable seccomp and AppArmor (their default filters block syscalls
// runc uses to create namespaces and pivot_root the inner container).
// /dev/fuse is the only device exposed.
func runSandboxPod(t *testing.T, agentTar, specPath, auditDir string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	containerName := fmt.Sprintf("sandbox-pod-e2e-%d", time.Now().UnixNano())

	args := []string{
		"run", "--rm",
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
		// Publish the agent's ingress listener so the host can POST in.
		// 18000:18000 is matched by agent.py's HTTPServer binding.
		"-p", "18000:18000",
		"-v", auditDir + ":/audit-out",
		"-v", agentTar + ":/mnt/agent.tar:ro",
		"-v", specPath + ":/mnt/spec.yaml:ro",
		sandboxRuntimeImage,
		"--spec", "/mnt/spec.yaml",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)

	var out bytes.Buffer
	idleSeen := make(chan struct{})
	w := &lineSentinel{
		out: &out,
		// agent.py prints "DONE" once it has completed every probe (just
		// before entering its sleep loop). Wait for that one line.
		marker:   []byte("[agent:out] DONE"),
		minCount: 1,
		fired:    idleSeen,
	}
	cmd.Stdout, cmd.Stderr = w, w

	if err := cmd.Start(); err != nil {
		t.Fatalf("docker run start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

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
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "kill", containerName).Run()
		<-done
	case err := <-done:
		// Container exited before reaching the idle marker — that's a
		// failure (something crashed). Fall through with the partial
		// output so the assertions can produce diagnostics.
		t.Logf("container exited before agent reached idle: %v", err)
	case <-ctx.Done():
		_ = exec.Command("docker", "kill", containerName).Run()
		<-done
		t.Fatalf("container timed out after 90s\n%s", out.String())
	}
	return out.String()
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

// readJSONLines reads a file with one JSON object per line. Returns an
// empty slice if the file doesn't exist (so missing-file failures surface
// as missing-event assertions, not a fatal).
func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Errorf("open %s: %v", path, err)
		return nil
	}
	defer f.Close()
	var out []map[string]any
	dec := json.NewDecoder(f)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Errorf("decode %s: %v", path, err)
			break
		}
		out = append(out, m)
	}
	return out
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
