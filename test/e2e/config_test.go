package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sandbox-platform/agent-sandbox/internal/api"
	"github.com/sandbox-platform/agent-sandbox/internal/api/gen"
)

// TestConfigE2E exercises `GET /v1/config` and `PUT /v1/config`
// end-to-end against the mcp-server fixture, and verifies the
// load-bearing security invariant: the agent's egress to
// sandboxd's own API at 127.0.0.1:8080 is subject to the same
// allowlist as any other host. Default-deny by absence; only an
// explicit egress.allow entry naming 127.0.0.1 opens the door, and
// once the entry is removed via PUT the door closes again.
func TestConfigE2E(t *testing.T) {
	pod, session, ctx, cancel := startMcpFixture(t)
	defer pod.stop()
	defer cancel()
	defer session.Close()

	const apiURL = "http://127.0.0.1:8080"

	// GET returns the spec.yaml the pod was launched with, after
	// the spec.Spec → gen.SandboxConfig round-trip sandboxd does on
	// startup.
	initial := getConfig(t, apiURL)
	assertMountPresent(t, initial, "/workspace", gen.BackendGdrive)
	assertMountPresent(t, initial, "/scratch", gen.BackendLocal)
	assertEgressHostPresent(t, initial, "go.dev")

	// Default-deny: the agent can't curl the API because 127.0.0.1
	// isn't on the egress allowlist. sbxproxy serves a 403 in
	// response to the redirected HTTP request.
	assertAgentCurlStatus(t, ctx, session, "403")

	// Idempotent PUT: re-applying the same document succeeds with no
	// diff. The reconciler still fires a SIGHUP, but the proxy's
	// allowlist is unchanged so the agent's curl remains denied.
	noop := putConfig(t, apiURL, initial)
	if !noop.Applied {
		t.Fatalf("idempotent PUT: applied=false (error=%q)", derefString(noop.Error))
	}
	if noop.Changes.Fs != nil || noop.Changes.Egress != nil {
		t.Errorf("idempotent PUT produced changes: %+v", noop.Changes)
	}
	assertAgentCurlStatus(t, ctx, session, "403")

	// Add an explicit allow for 127.0.0.1:8080 → reconciler rewrites
	// the rules file and SIGHUPs sbxproxy → agent's curl now reaches
	// the API. Pinning Ports=[8080] keeps the door open only for the
	// API; if a follow-up wires another in-pod listener on a different
	// port (a future control plane, say), this rule won't accidentally
	// expose it. Reload is async, so poll.
	apiPorts := []int{8080}
	withAPIAllow := withExtraEgressRule(initial, gen.EgressRule{
		Host:  "127.0.0.1",
		Ports: &apiPorts,
	})
	add := putConfig(t, apiURL, withAPIAllow)
	if !add.Applied {
		t.Fatalf("add PUT: applied=false (error=%q)", derefString(add.Error))
	}
	assertEgressAdded(t, add.Changes, "127.0.0.1")
	eventuallyAgentCurlStatus(t, ctx, session, "200", 5*time.Second)

	// And the agent must see the same updated document the host wrote.
	agentSeen := mcpCurlConfig(t, ctx, session)
	assertEgressHostPresent(t, agentSeen, "127.0.0.1")
	assertEgressHostPresent(t, agentSeen, "go.dev")

	// Remove the rule → next reload → agent denied again.
	remove := putConfig(t, apiURL, initial)
	if !remove.Applied {
		t.Fatalf("remove PUT: applied=false (error=%q)", derefString(remove.Error))
	}
	assertEgressRemoved(t, remove.Changes, "127.0.0.1")
	eventuallyAgentCurlStatus(t, ctx, session, "403", 5*time.Second)

	// FS reconciliation: change /scratch's ACLs from "rw everywhere"
	// to "rw everywhere but deny /scratch/locked/**". sbxfuse must
	// pick up the new ACLs on SIGHUP and start denying writes under
	// /scratch/locked/ without restarting the mount.
	if exit := agentBashExit(t, ctx, session, "echo before > /scratch/precheck.txt"); exit != 0 {
		t.Fatalf("/scratch must be writable before fs reconcile (exit=%d)", exit)
	}
	withDeny := withScratchLockedDeny(initial)
	fsApply := putConfig(t, apiURL, withDeny)
	if !fsApply.Applied {
		t.Fatalf("fs PUT: applied=false (error=%q)", derefString(fsApply.Error))
	}
	if fsApply.Changes.Fs == nil {
		t.Fatalf("fs PUT: changes.fs is nil; want a diff: %+v", fsApply.Changes)
	}
	// Once the reconciler has SIGHUP'd sbxfuse, writing into
	// /scratch/locked/ fails. fusefs returns ENOENT on deny, so
	// `touch` exits non-zero. /scratch itself stays writable —
	// only the locked subtree is gated.
	eventuallyAgentBashFails(t, ctx, session,
		"mkdir -p /scratch/locked && touch /scratch/locked/x.txt",
		5*time.Second)
	if exit := agentBashExit(t, ctx, session, "echo still-ok > /scratch/sibling.txt"); exit != 0 {
		t.Fatalf("/scratch/sibling.txt write should still succeed (exit=%d)", exit)
	}

	// Restore original ACLs → deny lifts → writes under /scratch/locked
	// succeed again.
	if r := putConfig(t, apiURL, initial); !r.Applied {
		t.Fatalf("fs restore PUT: applied=false (error=%q)", derefString(r.Error))
	}
	eventuallyAgentBashSucceeds(t, ctx, session,
		"mkdir -p /scratch/locked && touch /scratch/locked/y.txt",
		5*time.Second)

	// Schema-invalid bodies are rejected with 400 by the OpenAPI
	// validator before the handler runs.
	assertPutStatus(t, apiURL, `{"fs": []}`, http.StatusBadRequest)
	assertPutStatus(t, apiURL, `{"fs": [{"mount": "relative", "backend": "local"}]}`, http.StatusBadRequest)
	assertPutStatus(t, apiURL, `{"fs": [{"mount": "/x", "backend": "bogus"}]}`, http.StatusBadRequest)
}

// withScratchLockedDeny returns a copy of cfg with a deny rule for
// /scratch/locked/** appended to the /scratch mount's ACL list. The
// rest of /scratch keeps its existing rw rules; the deny only fires
// for the locked subtree.
func withScratchLockedDeny(cfg gen.SandboxConfig) gen.SandboxConfig {
	out := cfg
	out.Fs = make([]gen.FileSystem, len(cfg.Fs))
	copy(out.Fs, cfg.Fs)
	for i := range out.Fs {
		if api.FSBase(out.Fs[i]).Mount != "/scratch" {
			continue
		}
		// /scratch is a local backend; unwrap, append the deny rule,
		// then re-marshal. Don't use FromLocalFileSystem — without a
		// discriminator mapping in the spec, oapi-codegen writes the
		// schema name ("LocalFileSystem") into the backend field
		// instead of the enum value "local", which would fail the
		// server's request validator.
		ls, err := out.Fs[i].AsLocalFileSystem()
		if err != nil {
			continue
		}
		var acls []gen.ACLRule
		if ls.Acls != nil {
			acls = append(acls, *ls.Acls...)
		}
		acls = append(acls, gen.ACLRule{Path: "/scratch/locked/**", Access: gen.Deny})
		ls.Acls = &acls
		b, err := json.Marshal(ls)
		if err != nil {
			continue
		}
		_ = out.Fs[i].UnmarshalJSON(b)
	}
	return out
}

// agentBashExit runs cmd inside the agent via the MCP bash tool and
// returns its exit code. Used by FS reconciliation checks to
// distinguish "write succeeded" from "ACL denied".
func agentBashExit(t *testing.T, ctx context.Context, session *mcp.ClientSession, cmd string) int {
	t.Helper()
	var out struct {
		Stdout, Stderr string
		ExitCode       int
	}
	callMCP(t, ctx, session, "bash", map[string]any{"cmd": cmd}, &out)
	return out.ExitCode
}

// eventuallyAgentBashFails polls until cmd starts exiting non-zero,
// or fails the test on timeout. The reconciler is async (PUT returns
// before sbxfuse has reloaded its ACLs) so the test can't assume a
// new policy is live the moment PUT commits.
func eventuallyAgentBashFails(t *testing.T, ctx context.Context, session *mcp.ClientSession, cmd string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	var last int
	for time.Now().Before(end) {
		last = agentBashExit(t, ctx, session, cmd)
		if last != 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agent bash %q: still exiting 0 after %v (want non-zero)", cmd, deadline)
}

// eventuallyAgentBashSucceeds is the sibling of
// eventuallyAgentBashFails: polls until cmd exits 0.
func eventuallyAgentBashSucceeds(t *testing.T, ctx context.Context, session *mcp.ClientSession, cmd string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	var last int
	for time.Now().Before(end) {
		last = agentBashExit(t, ctx, session, cmd)
		if last == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agent bash %q: still exiting %d after %v (want 0)", cmd, last, deadline)
}

func getConfig(t *testing.T, baseURL string) gen.SandboxConfig {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/config")
	if err != nil {
		t.Fatalf("GET /v1/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/config: status %d, body %s", resp.StatusCode, body)
	}
	var cfg gen.SandboxConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatalf("GET /v1/config decode: %v", err)
	}
	return cfg
}

func putConfig(t *testing.T, baseURL string, cfg gen.SandboxConfig) gen.ApplyResult {
	t.Helper()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, baseURL+"/v1/config", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /v1/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT /v1/config: status %d, body %s\nreq body: %s", resp.StatusCode, respBody, body)
	}
	var result gen.ApplyResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("PUT /v1/config decode: %v", err)
	}
	return result
}

func assertPutStatus(t *testing.T, baseURL, jsonBody string, want int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, baseURL+"/v1/config", strings.NewReader(jsonBody))
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /v1/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("PUT %s: status %d, want %d (body=%s)", jsonBody, resp.StatusCode, want, body)
	}
}

// agentCurlStatus runs `curl -s -o /dev/null -w '%{http_code}'` against
// the API from inside the agent container via the MCP bash tool, and
// returns the HTTP status that sbxproxy (or the upstream, if allowed)
// produced. 403 means the proxy denied; 200 means the request reached
// the API and the API answered. We never use `curl -f` here because we
// want to distinguish "proxy denied" from "couldn't even connect".
func agentCurlStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession) string {
	t.Helper()
	var out struct {
		Stdout, Stderr string
		ExitCode       int
	}
	callMCP(t, ctx, session, "bash", map[string]any{
		"cmd": "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/v1/config",
	}, &out)
	if out.ExitCode != 0 {
		t.Fatalf("agent curl: exit=%d stderr=%q stdout=%q", out.ExitCode, out.Stderr, out.Stdout)
	}
	return strings.TrimSpace(out.Stdout)
}

func assertAgentCurlStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession, want string) {
	t.Helper()
	got := agentCurlStatus(t, ctx, session)
	if got != want {
		t.Errorf("agent curl status: got %q, want %q", got, want)
	}
}

// eventuallyAgentCurlStatus polls agentCurlStatus until it equals want
// or the deadline expires. Proxy reloads are async (sandboxd's
// reconciler runs in a goroutine and SIGHUPs sbxproxy after a PUT
// commits) so the test can't assume the new ruleset is live the moment
// PUT returns.
func eventuallyAgentCurlStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession, want string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	var got string
	for time.Now().Before(end) {
		got = agentCurlStatus(t, ctx, session)
		if got == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agent curl status: got %q, want %q within %v", got, want, deadline)
}

// mcpCurlConfig fetches /v1/config from inside the agent and decodes
// it. The caller is responsible for ensuring the agent is allowed to
// reach the API; this helper just runs the request.
func mcpCurlConfig(t *testing.T, ctx context.Context, session *mcp.ClientSession) gen.SandboxConfig {
	t.Helper()
	var out struct {
		Stdout, Stderr string
		ExitCode       int
	}
	callMCP(t, ctx, session, "bash", map[string]any{
		"cmd": "curl -sf http://127.0.0.1:8080/v1/config",
	}, &out)
	if out.ExitCode != 0 {
		t.Fatalf("mcp curl body: exit=%d stderr=%q stdout=%q", out.ExitCode, out.Stderr, out.Stdout)
	}
	var cfg gen.SandboxConfig
	if err := json.Unmarshal([]byte(out.Stdout), &cfg); err != nil {
		t.Fatalf("mcp curl body decode %q: %v", out.Stdout, err)
	}
	return cfg
}

// withExtraEgressRule returns a copy of cfg with rule appended to the
// allow list. cfg is left untouched so callers can reuse it as the
// "before" snapshot when undoing a change.
func withExtraEgressRule(cfg gen.SandboxConfig, rule gen.EgressRule) gen.SandboxConfig {
	out := cfg
	var allow []gen.EgressRule
	if cfg.Egress != nil && cfg.Egress.Allow != nil {
		allow = append(allow, *cfg.Egress.Allow...)
	}
	allow = append(allow, rule)
	out.Egress = &gen.Egress{Allow: &allow}
	return out
}

func assertMountPresent(t *testing.T, cfg gen.SandboxConfig, mount string, backend gen.Backend) {
	t.Helper()
	for _, f := range cfg.Fs {
		base := api.FSBase(f)
		if base.Mount == mount {
			if base.Backend != backend {
				t.Errorf("mount %q: backend=%q, want %q", mount, base.Backend, backend)
			}
			return
		}
	}
	t.Errorf("mount %q not present in fs (got %+v)", mount, cfg.Fs)
}

func assertEgressHostPresent(t *testing.T, cfg gen.SandboxConfig, host string) {
	t.Helper()
	if egressHasHost(cfg, host) {
		return
	}
	var hosts []string
	if cfg.Egress != nil && cfg.Egress.Allow != nil {
		for _, r := range *cfg.Egress.Allow {
			hosts = append(hosts, r.Host)
		}
	}
	t.Errorf("egress host %q not in allow list (got %v)", host, hosts)
}

func egressHasHost(cfg gen.SandboxConfig, host string) bool {
	if cfg.Egress == nil || cfg.Egress.Allow == nil {
		return false
	}
	for _, r := range *cfg.Egress.Allow {
		if r.Host == host {
			return true
		}
	}
	return false
}

func assertEgressAdded(t *testing.T, ch gen.Changes, host string) {
	t.Helper()
	if ch.Egress == nil || ch.Egress.Added == nil {
		t.Errorf("changes.egress.added is empty; want host=%q", host)
		return
	}
	for _, r := range *ch.Egress.Added {
		if r.Host == host {
			return
		}
	}
	t.Errorf("changes.egress.added missing host %q (got %+v)", host, *ch.Egress.Added)
}

func assertEgressRemoved(t *testing.T, ch gen.Changes, host string) {
	t.Helper()
	if ch.Egress == nil || ch.Egress.Removed == nil {
		t.Errorf("changes.egress.removed is empty; want host=%q", host)
		return
	}
	for _, r := range *ch.Egress.Removed {
		if r.Host == host {
			return
		}
	}
	t.Errorf("changes.egress.removed missing host %q (got %+v)", host, *ch.Egress.Removed)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
