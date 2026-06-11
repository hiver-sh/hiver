package e2e_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
		headers     http.Header
		queryToken  string
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
