package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestMultiTenantIngressE2E verifies that ingress (the /proxy/<port> reverse
// proxy) reaches the CORRECT tenant when multiple sandboxes share one pack pod.
//
// Each packed sandbox runs in its own netns, so its workload is reachable from
// the pod netns only at the sandbox's guest IP — not 127.0.0.1, which would land
// on the pod's own loopback (or, worse, whichever tenant happened to bind it).
// Both sandboxes here bind an HTTP server on the SAME port (9000) but serve
// distinct bodies, so the only way A's proxy can return A's body (and never B's)
// is if ingress is routed per-tenant by guest IP rather than by port.
//
// Asserts:
//   - A's proxy URL returns A's body; B's returns B's body;
//   - isolation: A's proxy never returns B's body and vice versa.
//
// Requires the controller in pack mode (HIVE_PACK=1); skips otherwise (the 1:1
// controller puts each key in its own pod, so there is no shared-container case).
func TestMultiTenantIngressE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	const image = "python"
	const port = 9000 // both tenants bind the SAME port on purpose
	ts := time.Now().UnixNano()
	keyA := fmt.Sprintf("packing-a-%d", ts)
	keyB := fmt.Sprintf("packing-b-%d", ts)

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(3*time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mk := func(key string) *hiverclient.Sandbox {
		cfg := hiverclient.SandboxConfig{
			Image:      image,
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			FS: []hiverclient.FileSystem{
				{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/workspace/**", Access: "rw"}}},
			},
		}
		sbx, err := c.GetOrCreateSandbox(ctx, key, cfg)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox(%s): %v", key, err)
		}
		return sbx
	}

	sbxA := mk(keyA)
	sbxB := mk(keyB)
	// Tear each sandbox down via its own API (no controller involvement).
	t.Cleanup(func() {
		_ = sbxA.Shutdown(context.Background())
		_ = sbxB.Shutdown(context.Background())
	})

	// Both same-image keys must share ONE pod for the shared-container case to hold.
	if sbxA.ID != sbxB.ID {
		t.Skipf("keys landed in different pods (%s vs %s) — controller not in pack mode (HIVE_PACK=1)", sbxA.ID, sbxB.ID)
	}
	t.Logf("both keys packed into pod %s", sbxA.ID)

	exec := func(sbx *hiverclient.Sandbox, sh string) hiverclient.ExecResult {
		r, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: []string{"sh", "-c", sh}})
		if err != nil {
			t.Fatalf("exec %q: %v", sh, err)
		}
		return *r
	}

	// Start a backgrounded HTTP server in each sandbox that serves a one-line body
	// uniquely identifying the tenant. setsid + </dev/null detaches it from the
	// exec session so it keeps serving after exec returns. Bind 0.0.0.0 so the
	// pod-side reverse proxy can reach it across the veth at the guest IP.
	startServer := func(sbx *hiverclient.Sandbox, body string) {
		sh := fmt.Sprintf(
			"mkdir -p /tmp/www && printf '%%s' '%s' > /tmp/www/id.txt && "+
				"setsid python3 -m http.server %d --bind 0.0.0.0 --directory /tmp/www "+
				">/tmp/srv.log 2>&1 </dev/null & echo started",
			body, port)
		if r := exec(sbx, sh); !strings.Contains(r.Stdout, "started") {
			t.Fatalf("start server (body=%s): stdout=%q stderr=%q exit=%d", body, r.Stdout, r.Stderr, r.ExitCode)
		}
	}

	startServer(sbxA, "tenant-A")
	startServer(sbxB, "tenant-B")

	// Fetch /id.txt through a sandbox's ingress proxy, polling until the freshly
	// launched server answers (startup latency). Returns the response body.
	fetch := func(sbx *hiverclient.Sandbox) (string, error) {
		url := sbx.ProxyURL(port) + "id.txt"
		deadline := time.Now().Add(30 * time.Second)
		var lastErr error
		for time.Now().Before(deadline) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return "", err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				lastErr = err
				time.Sleep(500 * time.Millisecond)
				continue
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return strings.TrimSpace(string(data)), nil
			}
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
			time.Sleep(500 * time.Millisecond)
		}
		return "", lastErr
	}

	gotA, err := fetch(sbxA)
	if err != nil {
		t.Fatalf("ingress to A: %v", err)
	}
	gotB, err := fetch(sbxB)
	if err != nil {
		t.Fatalf("ingress to B: %v", err)
	}

	// Each tenant's proxy must hit its own workload, never the other's — even
	// though both bind the same port. A cross-hit (gotA == "tenant-B") is exactly
	// the failure mode of dialing 127.0.0.1 instead of the per-sandbox guest IP.
	if gotA != "tenant-A" {
		t.Errorf("ingress to A returned %q, want tenant-A", gotA)
	}
	if gotB != "tenant-B" {
		t.Errorf("ingress to B returned %q, want tenant-B", gotB)
	}
}
