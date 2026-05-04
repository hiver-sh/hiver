// Package e2e runs the full sandbox-pod prototype end-to-end on the host,
// not inside the container. The test:
//
//  1. Builds the language-agnostic sandbox-runtime image (root Dockerfile)
//     — sandboxd + sbxproxy + sbxfuse + runc, no language runtime.
//  2. Builds the agent-python image (test/e2e/fixtures/agent-python) — a
//     SEPARATE image containing python3 + agent.py with the script as
//     ENTRYPOINT.
//  3. `docker save`s the agent image into a tarball that gets bind-mounted
//     into the sandbox-pod container. sandboxd unpacks it into an OCI
//     rootfs and runs it under runc, sharing the sandbox-pod's netns and
//     bind-mounting the FUSE /workspace.
//  4. Starts two HTTP upstream servers on the host — one allowlisted, one
//     not — reachable from inside the sandbox-pod via host-gateway.
//  5. Loads test/e2e/fixtures/agent-python/spec.yaml, substitutes the
//     runtime-only fields (URLs from steps 4), and bind-mounts the result
//     at /mnt/spec.yaml. ALLOW_URL/DENY_URL/DENY_PATH ride into the
//     agent through agent.env (the spec's runc-aware forwarding hook).
//  6. Runs the sandbox-pod container, captures stdout/stderr, asserts on
//     (a) agent script output, (b) proxy audit log, (c) FUSE audit log,
//     (d) sandboxd's own lifecycle log lines.
//
// Skips automatically when Docker isn't available — runs anywhere a Docker
// daemon is reachable (Linux directly, macOS via Docker Desktop / OrbStack).
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
)

const (
	moduleRoot          = "../.."
	sandboxRuntimeImage = "sandbox-runtime"          // :latest — sandboxd + sidecars + runc
	agentImage          = "sandbox-agent-python:e2e" // separate image; ENTRYPOINT is python3 /agent.py
	// Pinned ports for the host-side upstream HTTP servers. They have
	// to be fixed because the spec.yaml fixture references them
	// literally (no template substitution). Picked high enough to
	// avoid common dev-tool collisions.
	upstreamAllowedPort = 17080
	upstreamDeniedPort  = 17081
)

func TestSandboxPodE2E(t *testing.T) {
	requireDocker(t)

	// The spec.yaml is the source of truth: it names where the agent
	// image is built from (agent.path, relative to the spec file). We
	// load it first so buildImages knows what to build.
	specPath, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures/agent-python/spec.yaml"))
	if err != nil {
		t.Fatalf("abs spec path: %v", err)
	}
	sp, err := spec.Load(specPath)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	agentDir := sp.Agent.Path
	if !filepath.IsAbs(agentDir) {
		agentDir = filepath.Join(filepath.Dir(specPath), agentDir)
	}

	buildImages(t, agentDir)
	agentTar := saveAgentImage(t)

	// Start the two host-side upstreams on the ports the fixture spec
	// pins. One is allowlisted (host-aliased to "upstream-allowed"),
	// the other isn't.
	stopUpstream := startUpstreams(t)
	defer stopUpstream()

	auditDir := t.TempDir()

	output := runSandboxPod(t, agentTar, specPath, auditDir)

	// (1) sandboxd-emitted operation log. The agent itself is silent —
	// sandboxd reports what it observed via the proxy and FUSE audit
	// streams. Each "agent op | …" line is one mediated operation.
	ops := parseAgentOps(output)
	for _, want := range []struct {
		desc      string
		substring string
	}{
		{"http allow", "proxy allow GET upstream-allowed"},
		{"http deny", "proxy deny  GET upstream-denied"},
		{"fs write hello.txt", "fuse  allow write      /hello.txt"},
		{"fs open hello.txt (read side)", "fuse  allow open       /hello.txt"},
		{"fs read hello.txt", "fuse  allow read       /hello.txt"},
		{"fs deny on /secret/...", "fuse  deny  lookup     /secret"},
	} {
		if !containsAny(ops, want.substring) {
			t.Errorf("missing %s op: no line containing %q", want.desc, want.substring)
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
// image (at agentDir, taken from spec.agent.path). They're not layered
// — each is its own root.
func buildImages(t *testing.T, agentDir string) {
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
// of the agent image. sandboxd unpacks this tar into an OCI rootfs at
// container start. The path is returned so the test can bind-mount it.
func saveAgentImage(t *testing.T) string {
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
	allowedSrv := mkServer(upstreamAllowedPort, func(w http.ResponseWriter, _ *http.Request) {
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
// --privileged is needed for runc to create namespaces and set up cgroups
// for the nested agent container; tightening this is a follow-up.
func runSandboxPod(t *testing.T, agentTar, specPath, auditDir string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	containerName := fmt.Sprintf("sandbox-pod-e2e-%d", time.Now().UnixNano())

	args := []string{
		"run", "--rm",
		"--name", containerName,
		"--privileged",
		"--device", "/dev/fuse",
		"--add-host", "upstream-allowed:host-gateway",
		"--add-host", "upstream-denied:host-gateway",
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
		out:      &out,
		marker:   []byte("sandboxd: agent op |"),
		minCount: 5, // 2 proxy verdicts + 3 FUSE ops in the standard probe set
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
		// Agent has emitted everything we want to assert on. Stop the
		// pod so the test can finish; --rm cleans up the container.
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

// parseAgentOps extracts the trailing "…" portion of each
// "sandboxd: agent op | …" line that sandboxd emits as it tails the
// proxy and FUSE audit streams. The agent itself produces no logs;
// this is the orchestrator's view of what the agent did.
func parseAgentOps(stream string) []string {
	const marker = "sandboxd: agent op | "
	var out []string
	for _, line := range strings.Split(stream, "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		out = append(out, line[i+len(marker):])
	}
	return out
}

// containsAny returns true if any of lines contains substr.
func containsAny(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
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
