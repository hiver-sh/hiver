package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEgressACLE2E verifies the egress allowlist for both plain HTTP and
// HTTPS (TLS) traffic.
//
// HTTP: the agent's access to the hive gateway (a routed, plain-HTTP endpoint
// on the Docker network) is default-deny; adding an explicit allow rule opens
// the door and removing it closes it again. sbxproxy returns 403 for denied
// plain HTTP requests. The gateway is used rather than a sandbox-loopback
// address because a packed sandbox's loopback traffic stays in its own netns
// and is never redirected to sbxproxy, so loopback egress isn't ACL-gated.
//
// HTTPS: go.dev is initially allowed; removing it from the egress list
// blocks TLS connections to it (the proxy denies the CONNECT tunnel), and
// restoring the rule lets curl reach it again.
func TestEgressACLE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	bundleImage := setup.BuildTSMCPServerBundle(t)
	key := fmt.Sprintf("e2e-egress-%d", time.Now().UnixNano())

	config := hiverclient.SandboxConfig{
		Image: bundleImage,
		FS: []hiverclient.FileSystem{
			{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
		},
		Egress: []hiverclient.EgressRule{
			{Access: "allow", Host: "go.dev"},
		},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}
	// Tear the sandbox down via its own API (no controller involvement).
	t.Cleanup(func() { _ = sbx.Shutdown(context.Background()) })

	session := setup.ConnectMCP(t, ctx, sbx.ProxyURL(3000)+"mcp", &bytes.Buffer{})
	defer session.Close()

	initial, err := sbx.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	assertEgressHostPresent(t, *initial, "go.dev")

	// Default-deny: the gateway is not on the allowlist → sbxproxy returns 403.
	assertAgentCurlStatus(t, ctx, session, "403")

	// Idempotent PUT: re-applying the same document produces no diff.
	noop, err := sbx.ApplyConfig(ctx, *initial)
	if err != nil {
		t.Fatalf("idempotent PUT: %v", err)
	}
	if !noop.Applied {
		t.Fatalf("idempotent PUT: applied=false (error=%q)", noop.Error)
	}
	if noop.Changes.FS != nil || noop.Changes.Egress != nil {
		t.Errorf("idempotent PUT produced changes: %+v", noop.Changes)
	}
	assertAgentCurlStatus(t, ctx, session, "403")

	// Add an explicit allow for the gateway → proxy reloads → 200.
	withAPIAllow := withExtraEgressRule(*initial, hiverclient.EgressRule{
		Access: "allow",
		Host:   agentGatewayHost,
		Ports:  []int{agentGatewayPort},
	})
	add, err := sbx.ApplyConfig(ctx, withAPIAllow)
	if err != nil {
		t.Fatalf("add egress PUT: %v", err)
	}
	if !add.Applied {
		t.Fatalf("add egress PUT: applied=false (error=%q)", add.Error)
	}
	assertEgressAdded(t, add.Changes, agentGatewayHost)
	eventuallyAgentCurlStatus(t, ctx, session, "200", 5*time.Second)

	applied, err := sbx.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig after add: %v", err)
	}
	assertEgressHostPresent(t, *applied, agentGatewayHost)
	assertEgressHostPresent(t, *applied, "go.dev")

	// Remove the gateway rule → proxy reloads → 403.
	remove, err := sbx.ApplyConfig(ctx, *initial)
	if err != nil {
		t.Fatalf("remove egress PUT: %v", err)
	}
	if !remove.Applied {
		t.Fatalf("remove egress PUT: applied=false (error=%q)", remove.Error)
	}
	assertEgressRemoved(t, remove.Changes, agentGatewayHost)
	eventuallyAgentCurlStatus(t, ctx, session, "403", 5*time.Second)

	// go.dev is still in the allowlist → CONNECT tunnel is permitted → curl exits 0.
	if exit := agentBashExit(t, ctx, session, "curl -s -o /dev/null --max-time 10 https://go.dev"); exit != 0 {
		t.Fatalf("HTTPS to allowed go.dev should succeed (exit=%d)", exit)
	}

	// Remove go.dev → proxy denies the CONNECT tunnel → curl exits non-zero.
	denyAll, err := sbx.ApplyConfig(ctx, hiverclient.SandboxConfig{
		Image:  initial.Image,
		FS:     initial.FS,
		Egress: []hiverclient.EgressRule{},
	})
	if err != nil {
		t.Fatalf("deny-all egress PUT: %v", err)
	}
	if !denyAll.Applied {
		t.Fatalf("deny-all egress PUT: applied=false (error=%q)", denyAll.Error)
	}
	eventuallyAgentBashFails(t, ctx, session,
		"curl -s -o /dev/null --max-time 10 https://go.dev",
		10*time.Second)

	// Restore go.dev → CONNECT permitted again.
	if r, err := sbx.ApplyConfig(ctx, *initial); err != nil {
		t.Fatalf("restore egress PUT: %v", err)
	} else if !r.Applied {
		t.Fatalf("restore egress PUT: applied=false (error=%q)", r.Error)
	}
	eventuallyAgentBashSucceeds(t, ctx, session,
		"curl -s -o /dev/null --max-time 10 https://go.dev",
		10*time.Second)

	// ── Invalid config rejection ──────────────────────────────────────────────

	for _, invalid := range []hiverclient.SandboxConfig{
		{FS: []hiverclient.FileSystem{{Mount: "relative", Backend: "local"}}},
		{FS: []hiverclient.FileSystem{{Mount: "/x", Backend: "bogus"}}},
	} {
		result, err := sbx.ApplyConfig(ctx, invalid)
		if err == nil && result.Applied {
			t.Errorf("ApplyConfig(%+v): want rejection, got applied=true", invalid)
		}
	}
}

// TestEgressDNSSinkholeE2E verifies that DNS is sinkholed: every name the agent
// resolves — allowlisted or not — comes back as the constant placeholder, so a
// resolution carries no data off-box and DNS can't be an exfil tunnel. An
// allowlisted host is still reachable because the proxy re-resolves the real
// name itself after the agent connects to the placeholder.
func TestEgressDNSSinkholeE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	bundleImage := setup.BuildTSMCPServerBundle(t)
	key := fmt.Sprintf("e2e-dns-%d", time.Now().UnixNano())

	config := hiverclient.SandboxConfig{
		Image: bundleImage,
		FS: []hiverclient.FileSystem{
			{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
		},
		Egress: []hiverclient.EgressRule{
			{Access: "allow", Host: "go.dev"},
		},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}
	// Tear the sandbox down via its own API (no controller involvement).
	t.Cleanup(func() { _ = sbx.Shutdown(context.Background()) })

	session := setup.ConnectMCP(t, ctx, sbx.ProxyURL(3000)+"mcp", &bytes.Buffer{})
	defer session.Close()

	const sinkIP = "11.0.0.1"

	// Every name resolves to the constant placeholder, regardless of whether it
	// is on the allowlist: an allowlisted host (go.dev), an unlisted host, and
	// an exfil-shaped name all return the sink, so the agent's resolver never
	// reaches a real authoritative server with attacker-chosen labels.
	for _, name := range []string{
		"go.dev",
		"unlisted-host.example.com",
		"secret-data-exfil.attacker.example",
	} {
		if got := agentResolve(t, ctx, session, name); got != sinkIP {
			t.Errorf("DNS for %q = %q, want sink %q", name, got, sinkIP)
		}
	}

	// Despite resolving to the placeholder, an allowlisted host is still
	// reachable: the agent connects to the placeholder, the proxy reads the SNI
	// and re-resolves go.dev on its own resolver to make the real connection.
	if exit := agentBashExit(t, ctx, session, "curl -s -o /dev/null --max-time 10 https://go.dev"); exit != 0 {
		t.Fatalf("HTTPS to allowed go.dev should succeed despite sinkholed DNS (exit=%d)", exit)
	}
}

// agentResolve resolves name from inside the sandbox via Node's getaddrinfo
// (dns.lookup), returning the IPv4 address the agent's resolver hands back. Node
// is used because the alpine agent image ships no getent/dig but always has a
// Node runtime. With `node -e <script> <name>`, name lands in process.argv[1].
func agentResolve(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string) string {
	t.Helper()
	// Single-quote-free so the whole script can be single-quoted for the shell.
	const script = `require("dns").lookup(process.argv[1], {family: 4}, (e, a) => { if (e) { console.error(e.message); process.exit(1); } process.stdout.write(a); });`
	var out struct {
		Stdout, Stderr string
		ExitCode       int
	}
	setup.CallMCP(t, ctx, session, "bash", map[string]any{
		"cmd": fmt.Sprintf(`node -e '%s' '%s'`, script, name),
	}, &out)
	if out.ExitCode != 0 {
		t.Fatalf("agent resolve %q: exit=%d stderr=%q", name, out.ExitCode, out.Stderr)
	}
	return strings.TrimSpace(out.Stdout)
}

// agentGatewayHost / agentGatewayPort are the hive gateway as the sandbox
// reaches it on the internal Docker network (the `gateway` compose service,
// injected as HIVER_GATEWAY_URL). It is a plain-HTTP endpoint reachable via
// routed egress, so it exercises the egress allowlist through sbxproxy — unlike
// a sandbox-loopback target (127.0.0.1), which a packed sandbox's egress rules
// never see (loopback stays in its netns and is not redirected to the proxy).
const (
	agentGatewayHost = "gateway"
	agentGatewayPort = 10000
	// agentGatewayPath is a controller endpoint reachable through the gateway that
	// returns 200 (the sandbox listing) once egress to the gateway is allowed.
	agentGatewayPath = "/controller/v1/sandboxes"
)

// agentGatewayURL is a plain-HTTP endpoint that returns 200 once egress to the
// gateway is allowed, so its status doubles as an allow/deny probe: 200 when the
// proxy forwards, 403 when it denies.
var agentGatewayURL = fmt.Sprintf("http://%s:%d%s", agentGatewayHost, agentGatewayPort, agentGatewayPath)

func agentCurlStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession) string {
	t.Helper()
	var out struct {
		Stdout, Stderr string
		ExitCode       int
	}
	setup.CallMCP(t, ctx, session, "bash", map[string]any{
		"cmd": fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' %s", agentGatewayURL),
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

func withExtraEgressRule(cfg hiverclient.SandboxConfig, rule hiverclient.EgressRule) hiverclient.SandboxConfig {
	out := cfg
	out.Egress = append(append([]hiverclient.EgressRule(nil), cfg.Egress...), rule)
	return out
}

func assertEgressHostPresent(t *testing.T, cfg hiverclient.SandboxConfig, host string) {
	t.Helper()
	if egressHasHost(cfg, host) {
		return
	}
	var hosts []string
	for _, r := range cfg.Egress {
		hosts = append(hosts, r.Host)
	}
	t.Errorf("egress host %q not in egress list (got %v)", host, hosts)
}

func egressHasHost(cfg hiverclient.SandboxConfig, host string) bool {
	for _, r := range cfg.Egress {
		if r.Host == host {
			return true
		}
	}
	return false
}

func assertEgressAdded(t *testing.T, ch hiverclient.Changes, host string) {
	t.Helper()
	if ch.Egress == nil || len(ch.Egress.Added) == 0 {
		t.Errorf("changes.egress.added is empty; want host=%q", host)
		return
	}
	for _, r := range ch.Egress.Added {
		if r.Host == host {
			return
		}
	}
	t.Errorf("changes.egress.added missing host %q (got %+v)", host, ch.Egress.Added)
}

func assertEgressRemoved(t *testing.T, ch hiverclient.Changes, host string) {
	t.Helper()
	if ch.Egress == nil || len(ch.Egress.Removed) == 0 {
		t.Errorf("changes.egress.removed is empty; want host=%q", host)
		return
	}
	for _, r := range ch.Egress.Removed {
		if r.Host == host {
			return
		}
	}
	t.Errorf("changes.egress.removed missing host %q (got %+v)", host, ch.Egress.Removed)
}
