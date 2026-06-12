package e2e_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestEgressRequestOverridesE2E verifies that the transparent egress proxy
// injects the headers and query params declared in an EgressRule.Override
// into outbound HTTP requests before they reach the upstream.
//
// The host starts an HTTP server on a random free port and records the first
// request it receives. The sandbox runs hiversh/python:3.13-alpine with
// `tail -f /dev/null` as the entrypoint. An Exec triggers a bare GET with no
// special headers or query params; the proxy's rule Override should inject:
//
//	headers["X-Injected-By"] = "proxy"
//	query["token"]           = "secret123"
//
// The host alias "upstream-override:host-gateway" is exposed to the sandbox
// via ExtraHosts so the sandbox can resolve the hostname. After Exec returns
// the test asserts the capture server received both injections.
func TestEgressRequestOverridesE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	// Bind the capture server on all interfaces so Docker can reach it via
	// the host-gateway alias.
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	capturePort := l.Addr().(*net.TCPAddr).Port

	type capturedReq struct {
		headers    http.Header
		queryToken string
	}
	// Buffered so the handler never blocks: the request arrives synchronously
	// inside the python urlopen call, which must complete before Exec returns.
	capturedCh := make(chan capturedReq, 1)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case capturedCh <- capturedReq{
				headers:    r.Header.Clone(),
				queryToken: r.URL.Query().Get("token"),
			}:
			default:
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	const hostAlias = "upstream-override"
	key := fmt.Sprintf("e2e-egress-overrides-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "hiversh/python:3.13-alpine",
		Entrypoint: "tail -f /dev/null",
		// Expose the host-side capture server hostname inside the Docker network.
		ExtraHosts: []string{
			hostAlias + ":host-gateway",
		},
		Egress: []hiverclient.EgressRule{
			{
				Access: "allow",
				Host:   hostAlias,
				Ports:  []int{capturePort},
				Override: &hiverclient.EgressOverride{
					Headers: map[string]string{"X-Injected-By": "proxy"},
					Query:   map[string]string{"token": "secret123"},
				},
			},
		},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))
	t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}

	// A bare GET with no special headers or query params. The proxy should
	// inject the Override before forwarding to the capture server.
	cmd := fmt.Sprintf(
		`python3 -c "import urllib.request; urllib.request.urlopen('http://%s:%d/probe', timeout=10)"`,
		hostAlias, capturePort,
	)
	result, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: cmd})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("python request failed (exit=%d stderr=%q): proxy may have denied the egress request",
			result.ExitCode, result.Stderr)
	}

	// By the time Exec returns the capture server has already handled the
	// request, so the channel is populated without any real wait.
	select {
	case cap := <-capturedCh:
		if got := cap.headers.Get("X-Injected-By"); got != "proxy" {
			t.Errorf("header X-Injected-By: got %q, want %q", got, "proxy")
		}
		if cap.queryToken != "secret123" {
			t.Errorf("query param token: got %q, want %q", cap.queryToken, "secret123")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture server did not receive the request")
	}
}

// TestEgressOverrideHostE2E verifies that an EgressRule.Override.Host
// substitutes the upstream the proxy dials while leaving the agent-visible
// request untouched, and that Override.PrefixPath prepends to the outbound
// path on the way through.
//
// The sandbox requests http://api.virtual.test/probe — a virtual hostname on
// a port (80) where nothing listens at its resolved address. The egress rule
// for that host carries override.host = "upstream-redirect:<capturePort>",
// where the alias maps to the host machine via ExtraHosts, so the only way
// the request can succeed is the proxy dialing the override target. The
// capture server must observe the original hostname in the Host header
// (virtual-host routing stays possible).
//
// api.virtual.test gets an ExtraHosts entry as well: it only needs to resolve
// to *something* so the agent's TCP connect happens and gets intercepted —
// with the DNS sinkhole every name resolves anyway, but the hosts entry keeps
// the test independent of that.
func TestEgressOverrideHostE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	capturePort := l.Addr().(*net.TCPAddr).Port

	type capturedReq struct {
		host           string
		path           string
		injectedHeader string
		queryToken     string
	}
	capturedCh := make(chan capturedReq, 1)

	// Returned to the agent so the test can verify the upstream's response
	// travels back through the proxy intact.
	const responseMarker = "served-by-override-target"
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case capturedCh <- capturedReq{
				host:           r.Host,
				path:           r.URL.Path,
				injectedHeader: r.Header.Get("X-Injected-By"),
				queryToken:     r.URL.Query().Get("token"),
			}:
			default:
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(responseMarker))
		}),
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	const agentHost = "api.virtual.test"
	const redirectAlias = "upstream-redirect"
	key := fmt.Sprintf("e2e-egress-override-host-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "hiversh/python:3.13-alpine",
		Entrypoint: "tail -f /dev/null",
		// The redirect alias is for the proxy's own resolution of the
		// override target; the agent never sees it. The agent host maps to
		// host-gateway only so the connect() succeeds — port 80 there has no
		// listener for this test, so reaching the capture server proves the
		// override.
		ExtraHosts: []string{
			redirectAlias + ":host-gateway",
			agentHost + ":host-gateway",
		},
		Egress: []hiverclient.EgressRule{
			{
				Access: "allow",
				Host:   agentHost,
				Override: &hiverclient.EgressOverride{
					Host:       fmt.Sprintf("%s:%d", redirectAlias, capturePort),
					PrefixPath: "/mock",
					Headers:    map[string]string{"X-Injected-By": "proxy"},
					Query:      map[string]string{"token": "secret123"},
				},
			},
		},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))
	t.Cleanup(func() { _ = c.Shutdown(context.Background(), key) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}

	cmd := fmt.Sprintf(
		`python3 -c "import urllib.request; print(urllib.request.urlopen('http://%s/probe', timeout=10).read().decode())"`,
		agentHost,
	)
	result, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: cmd})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("python request failed (exit=%d stderr=%q): proxy may not have dialed the override target",
			result.ExitCode, result.Stderr)
	}
	// The agent must see the override target's response body — proves the
	// full round trip (agent → proxy → override upstream → proxy → agent),
	// not just that the upstream was reached.
	if !strings.Contains(result.Stdout, responseMarker) {
		t.Errorf("agent stdout %q does not contain upstream response %q", result.Stdout, responseMarker)
	}

	select {
	case cap := <-capturedCh:
		if cap.host != agentHost {
			t.Errorf("capture server saw Host %q, want %q", cap.host, agentHost)
		}
		// The agent requested /probe; prefix_path namespaces it under /mock.
		if cap.path != "/mock/probe" {
			t.Errorf("capture server saw path %q, want %q", cap.path, "/mock/probe")
		}
		// The header and query overrides must compose with host + prefix.
		if cap.injectedHeader != "proxy" {
			t.Errorf("header X-Injected-By: got %q, want %q", cap.injectedHeader, "proxy")
		}
		if cap.queryToken != "secret123" {
			t.Errorf("query param token: got %q, want %q", cap.queryToken, "secret123")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture server did not receive the request")
	}
}
