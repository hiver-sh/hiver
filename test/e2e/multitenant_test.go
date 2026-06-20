package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestMultiTenantPackE2E verifies the multi-sandbox-per-container design (§3-4,
// §6, §10): N runc containers of the SAME image packed into ONE pod container,
// each a distinct keyed sandbox, exercising every feature end-to-end through the
// real controller → gateway → sandboxd stack:
//
//   - placement: same-image keys land in the SAME pod (one routing id);
//   - filesystem: per-key sbxfuse workspaces are isolated even though both keys
//     mount the same path (/workspace) — verified via exec AND the file API;
//   - egress: the shared sbxproxy enforces a DIFFERENT allowlist per key, keyed
//     by each sandbox's distinct source IP — verified with real DNS resolution;
//   - keyed API: exec, config, and info each resolve the addressed sandbox.
//
// Requires the controller in pack mode (HIVE_PACK=1); skips otherwise (the 1:1
// controller would put each key in its own container, so the same-container
// assertions can't hold).
func TestMultiTenantPackE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	const image = "hiversh/python:3.13-alpine"
	ts := time.Now().UnixNano()
	keyA := fmt.Sprintf("packa-%d", ts)
	keyB := fmt.Sprintf("packb-%d", ts)

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(3*time.Minute))
	t.Cleanup(func() {
		_ = c.Shutdown(context.Background(), keyA)
		_ = c.Shutdown(context.Background(), keyB)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mk := func(key, allowHost string) *hiverclient.Sandbox {
		cfg := hiverclient.SandboxConfig{
			Image:      image,
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			FS: []hiverclient.FileSystem{
				{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/workspace/**", Access: "rw"}}},
			},
			// example.com / example.org both serve a stable 200 without
			// cross-host redirects, so the allow/deny outcome is deterministic.
			Egress: []hiverclient.EgressRule{{Access: "allow", Host: allowHost}},
		}
		sbx, err := c.GetOrCreateSandbox(ctx, key, cfg)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox(%s): %v", key, err)
		}
		return sbx
	}

	sbxA := mk(keyA, "example.com")
	sbxB := mk(keyB, "example.org")

	// --- placement: both same-image keys must share ONE pod (one routing id) ---
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

	// --- keyed API: each sandbox resolves its own runtime info + config ---
	if info, err := sbxA.GetInfo(ctx); err != nil || info.Isolation != "container" {
		t.Errorf("keyA GetInfo = %+v, %v; want isolation=container", info, err)
	}
	if cfg, err := sbxB.GetConfig(ctx); err != nil || len(cfg.Egress) != 1 || cfg.Egress[0].Host != "example.org" {
		t.Errorf("keyB GetConfig egress = %+v, %v; want [example.org]", cfg, err)
	}

	// --- filesystem isolation: same /workspace path, distinct per-key backing ---
	exec(sbxA, "echo AAA > /workspace/secret.txt")
	exec(sbxB, "echo BBB > /workspace/secret.txt")

	if got := strings.TrimSpace(exec(sbxA, "cat /workspace/secret.txt").Stdout); got != "AAA" {
		t.Errorf("keyA /workspace/secret.txt via exec = %q, want AAA", got)
	}
	if got := strings.TrimSpace(exec(sbxB, "cat /workspace/secret.txt").Stdout); got != "BBB" {
		t.Errorf("keyB /workspace/secret.txt via exec = %q, want BBB", got)
	}
	// Read back through the file API too — it must hit the same per-key backend.
	if data, err := sbxA.ReadFile(ctx, "/workspace/secret.txt"); err != nil || strings.TrimSpace(string(data)) != "AAA" {
		t.Errorf("keyA ReadFile = %q, %v; want AAA", string(data), err)
	}
	if data, err := sbxB.ReadFile(ctx, "/workspace/secret.txt"); err != nil || strings.TrimSpace(string(data)) != "BBB" {
		t.Errorf("keyB ReadFile = %q, %v; want BBB", string(data), err)
	}
	// Each sees ONLY its own file (the same path, isolated backends).
	if ls := strings.TrimSpace(exec(sbxA, "ls /workspace").Stdout); ls != "secret.txt" {
		t.Errorf("keyA ls /workspace = %q, want only secret.txt", ls)
	}

	// Real DNS resolves through the per-sandbox sink to the proxy placeholder,
	// proving name resolution works inside a packed sandbox's own netns.
	if got := strings.TrimSpace(exec(sbxA, "python3 -c \"import socket;print(socket.gethostbyname('example.com'))\"").Stdout); got != "192.0.2.1" {
		t.Errorf("keyA DNS example.com = %q, want 192.0.2.1 (proxy placeholder)", got)
	}

	// --- egress isolation: each key's allowlist is enforced by source IP ---
	// Probe the proxy's decision directly: connect to the placeholder with a Host
	// header (single request, no redirect-following), so the result is purely the
	// per-source allow/deny — allow → upstream status (non-403), deny → proxy 403.
	probe := func(sbx *hiverclient.Sandbox, host string) string {
		py := fmt.Sprintf(
			"import http.client as h\n"+
				"try:\n"+
				"  c=h.HTTPConnection('192.0.2.1',80,timeout=15)\n"+
				"  c.request('GET','/',headers={'Host':'%s'})\n"+
				"  print('STATUS', c.getresponse().status)\n"+
				"except Exception as e: print('ERR', type(e).__name__)",
			host)
		r := exec(sbx, "python3 -c \""+strings.ReplaceAll(py, "\"", "\\\"")+"\"")
		return strings.TrimSpace(r.Stdout + r.Stderr)
	}
	allow := func(out string) bool { return strings.HasPrefix(out, "STATUS") && !strings.Contains(out, "403") }
	deny := func(out string) bool { return strings.Contains(out, "403") }

	if out := probe(sbxA, "example.com"); !allow(out) {
		t.Errorf("keyA → example.com (allowed) got %q, want allowed", out)
	}
	if out := probe(sbxA, "example.org"); !deny(out) {
		t.Errorf("keyA → example.org (NOT in keyA allowlist) got %q, want 403", out)
	}
	if out := probe(sbxB, "example.org"); !allow(out) {
		t.Errorf("keyB → example.org (allowed) got %q, want allowed", out)
	}
	if out := probe(sbxB, "example.com"); !deny(out) {
		t.Errorf("keyB → example.com (NOT in keyB allowlist) got %q, want 403", out)
	}
}

// TestMultiTenantApplyConfigMountE2E verifies that ApplyConfig correctly adds a new
// local filesystem mount to a packed sandbox while the workload is already running,
// and that each sandbox's mounts remain isolated from one another within the shared pod.
//
// Sequence:
//  1. Create sandboxes A and B in the same pod, each with /workspace only.
//  2. Apply a config update to A that adds /data-a; apply a different update to B
//     that adds /data-b.
//  3. Assert each sandbox can write to its new mount.
//  4. Assert the mounts are isolated: A cannot access /data-b, B cannot access /data-a.
//  5. Assert the original /workspace still works on both sandboxes after the apply.
func TestMultiTenantApplyConfigMountE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	const image = "hiversh/python:3.13-alpine"
	ts := time.Now().UnixNano()
	keyA := fmt.Sprintf("packcfg-a-%d", ts)
	keyB := fmt.Sprintf("packcfg-b-%d", ts)

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(3*time.Minute))
	t.Cleanup(func() {
		_ = c.Shutdown(context.Background(), keyA)
		_ = c.Shutdown(context.Background(), keyB)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	baseFS := []hiverclient.FileSystem{
		{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
	}
	mk := func(key string) *hiverclient.Sandbox {
		cfg := hiverclient.SandboxConfig{
			Image:      image,
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			FS:         baseFS,
		}
		sbx, err := c.GetOrCreateSandbox(ctx, key, cfg)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox(%s): %v", key, err)
		}
		return sbx
	}

	sbxA := mk(keyA)
	sbxB := mk(keyB)

	if sbxA.ID != sbxB.ID {
		t.Skipf("keys landed in different pods (%s vs %s) — controller not in pack mode (HIVE_PACK=1)", sbxA.ID, sbxB.ID)
	}
	t.Logf("both keys packed into pod %s", sbxA.ID)

	execSh := func(sbx *hiverclient.Sandbox, sh string) hiverclient.ExecResult {
		r, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: []string{"sh", "-c", sh}})
		if err != nil {
			t.Fatalf("exec(%q): %v", sh, err)
		}
		return *r
	}

	// Verify /workspace is writable on both sandboxes before the config change.
	if r := execSh(sbxA, "echo ok > /workspace/pre.txt"); r.ExitCode != 0 {
		t.Fatalf("A: /workspace not writable before apply (exit=%d)", r.ExitCode)
	}
	if r := execSh(sbxB, "echo ok > /workspace/pre.txt"); r.ExitCode != 0 {
		t.Fatalf("B: /workspace not writable before apply (exit=%d)", r.ExitCode)
	}

	// Apply a config to A that adds /data-a (a new local mount).
	cfgA := hiverclient.SandboxConfig{
		Image:      image,
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		FS: []hiverclient.FileSystem{
			{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
			{Mount: "/data-a", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
		},
	}
	resA, err := sbxA.ApplyConfig(ctx, cfgA)
	if err != nil {
		t.Fatalf("A ApplyConfig: %v", err)
	}
	if !resA.Applied {
		t.Fatalf("A ApplyConfig: applied=false (error=%q)", resA.Error)
	}

	// Apply a config to B that adds /data-b (a different new local mount).
	cfgB := hiverclient.SandboxConfig{
		Image:      image,
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		FS: []hiverclient.FileSystem{
			{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
			{Mount: "/data-b", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
		},
	}
	resB, err := sbxB.ApplyConfig(ctx, cfgB)
	if err != nil {
		t.Fatalf("B ApplyConfig: %v", err)
	}
	if !resB.Applied {
		t.Fatalf("B ApplyConfig: applied=false (error=%q)", resB.Error)
	}

	// Give the mount manager a moment to inject the new mounts into the live workload.
	time.Sleep(2 * time.Second)

	// A can write to /data-a.
	if r := execSh(sbxA, "echo hello > /data-a/test.txt && cat /data-a/test.txt"); r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "hello" {
		t.Errorf("A: /data-a not writable after apply (exit=%d, out=%q)", r.ExitCode, r.Stdout)
	}

	// B can write to /data-b.
	if r := execSh(sbxB, "echo world > /data-b/test.txt && cat /data-b/test.txt"); r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "world" {
		t.Errorf("B: /data-b not writable after apply (exit=%d, out=%q)", r.ExitCode, r.Stdout)
	}

	// Mount isolation: A's new mount must not be visible inside B and vice versa.
	if r := execSh(sbxA, "test -d /data-b"); r.ExitCode == 0 {
		t.Errorf("A: /data-b (B's mount) is unexpectedly visible inside A")
	}
	if r := execSh(sbxB, "test -d /data-a"); r.ExitCode == 0 {
		t.Errorf("B: /data-a (A's mount) is unexpectedly visible inside B")
	}

	// Original /workspace is still writable on both after the config change.
	if r := execSh(sbxA, "echo post > /workspace/post.txt"); r.ExitCode != 0 {
		t.Errorf("A: /workspace not writable after apply (exit=%d)", r.ExitCode)
	}
	if r := execSh(sbxB, "echo post > /workspace/post.txt"); r.ExitCode != 0 {
		t.Errorf("B: /workspace not writable after apply (exit=%d)", r.ExitCode)
	}
}

// TestMultiTenantSnapshotE2E verifies that two packed sandboxes sharing one pod
// container can each write and restore their own snapshot independently.
//
// Sequence:
//  1. Create write sandboxes A and B in the same pack pod (same image ensures
//     same-pod placement); assert they share a pod ID.
//  2. Each writes a unique marker file directly into its overlay upper layer.
//  3. Shut down A, then B — sandboxd captures each sandbox's overlay into an
//     independent snapshot tarball keyed by its own WriteKey.
//  4. Create restore sandboxes rA and rB (same image → same pack pod); assert
//     same-pod placement again.
//  5. Assert rA contains A's file with A's content (correct restore).
//  6. Assert rB contains B's file with B's content (correct restore).
//  7. Assert isolation: rA does not contain B's file and rB does not contain A's.
func TestMultiTenantSnapshotE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	const image = "hiversh/python:3.13-alpine"
	ts := time.Now().UnixNano()

	// Each sandbox gets a unique snapshot key; the write and restore sandboxes
	// use distinct API keys so the controller creates independent workloads.
	snapKeyA := fmt.Sprintf("mtsna-%d", ts)
	snapKeyB := fmt.Sprintf("mtsnb-%d", ts)
	writeKeyA := "w" + snapKeyA
	writeKeyB := "w" + snapKeyB
	restoreKeyA := "r" + snapKeyA
	restoreKeyB := "r" + snapKeyB

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(3*time.Minute))
	t.Cleanup(func() {
		_ = c.Shutdown(context.Background(), writeKeyA)
		_ = c.Shutdown(context.Background(), writeKeyB)
		_ = c.Shutdown(context.Background(), restoreKeyA)
		_ = c.Shutdown(context.Background(), restoreKeyB)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	mkWriter := func(key, snapKey string) *hiverclient.Sandbox {
		sbx, err := c.GetOrCreateSandbox(ctx, key, hiverclient.SandboxConfig{
			Image:      image,
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			Snapshot:   &hiverclient.Snapshot{WriteKey: snapKey},
		})
		if err != nil {
			t.Fatalf("GetOrCreateSandbox(%s): %v", key, err)
		}
		return sbx
	}

	// ── Phase 1: write ───────────────────────────────────────────────────────
	wA := mkWriter(writeKeyA, snapKeyA)
	wB := mkWriter(writeKeyB, snapKeyB)

	if wA.ID != wB.ID {
		t.Skipf("write sandboxes landed in different pods (%s vs %s) — controller not in pack mode (HIVE_PACK=1)", wA.ID, wB.ID)
	}
	t.Logf("write sandboxes packed into pod %s", wA.ID)

	exec := func(sbx *hiverclient.Sandbox, sh string) hiverclient.ExecResult {
		r, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: []string{"sh", "-c", sh}})
		if err != nil {
			t.Fatalf("exec %q: %v", sh, err)
		}
		return *r
	}

	// Write uniquely-named files into each sandbox's overlay upper layer so that
	// presence and absence checks are unambiguous after restore.
	exec(wA, "echo from-a > /marker-a.txt")
	exec(wB, "echo from-b > /marker-b.txt")

	// Shut down both write sandboxes. DELETE /v1/<key> returns immediately; the
	// teardown goroutine (StopAgent → snapshot capture → unmount) runs async.
	if err := wA.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown A: %v", err)
	}
	if err := wB.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown B: %v", err)
	}

	// Wait for both snapshot tarballs to appear on the host-side volume before
	// creating the restore sandboxes. The compose volume binds the container's
	// /snapshots to ~/.hive/snapshots, so the file is visible here once
	// sandboxd finishes the capture.
	snapshotDir := filepath.Join(os.Getenv("HOME"), ".hive", "snapshots")
	waitSnapshot := func(key string) {
		path := filepath.Join(snapshotDir, "snapshot-"+key+".tar.gz")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("snapshot for key %q not found at %s after 30s", key, path)
	}
	waitSnapshot(snapKeyA)
	waitSnapshot(snapKeyB)

	// ── Phase 2: restore ─────────────────────────────────────────────────────
	mkRestore := func(key, snapKey string) *hiverclient.Sandbox {
		sbx, err := c.GetOrCreateSandbox(ctx, key, hiverclient.SandboxConfig{
			Image:      image,
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			Snapshot:   &hiverclient.Snapshot{RestoreKey: snapKey},
		})
		if err != nil {
			t.Fatalf("GetOrCreateSandbox(%s): %v", key, err)
		}
		return sbx
	}

	rA := mkRestore(restoreKeyA, snapKeyA)
	rB := mkRestore(restoreKeyB, snapKeyB)

	if rA.ID != rB.ID {
		t.Skipf("restore sandboxes landed in different pods (%s vs %s) — controller not in pack mode (HIVE_PACK=1)", rA.ID, rB.ID)
	}
	t.Logf("restore sandboxes packed into pod %s", rA.ID)

	// rA must have A's file with the correct content.
	if got := strings.TrimSpace(exec(rA, "cat /marker-a.txt").Stdout); got != "from-a" {
		t.Errorf("rA: /marker-a.txt = %q, want from-a", got)
	}
	// rB must have B's file with the correct content.
	if got := strings.TrimSpace(exec(rB, "cat /marker-b.txt").Stdout); got != "from-b" {
		t.Errorf("rB: /marker-b.txt = %q, want from-b", got)
	}

	// Isolation: each sandbox's snapshot must not contain the other's file.
	if r := exec(rA, "test -f /marker-b.txt"); r.ExitCode == 0 {
		t.Errorf("rA: /marker-b.txt (B-only file) must not exist after restoring A's snapshot")
	}
	if r := exec(rB, "test -f /marker-a.txt"); r.ExitCode == 0 {
		t.Errorf("rB: /marker-a.txt (A-only file) must not exist after restoring B's snapshot")
	}
}

// TestMultiTenantEgressEventsE2E verifies that egress events flow to the correct
// per-sandbox broker when multiple sandboxes share one pack pod. This exercises the
// proxyRouter: sbxproxy audit events carry src_ip and are dispatched to the owning
// sandbox's broker rather than a single shared broker.
//
// Two sandboxes (A allows example.com, B allows example.org) are created in the same
// pod. Each sandbox makes one allowed and one denied request, then we collect each
// sandbox's event stream independently and assert:
//   - A's stream has allowed(example.com) + denied(example.org)
//   - B's stream has allowed(example.org) + denied(example.com)
//   - No cross-contamination (A never sees B's allowed events and vice versa)
func TestMultiTenantEgressEventsE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	const image = "hiversh/python:3.13-alpine"
	ts := time.Now().UnixNano()
	keyA := fmt.Sprintf("packev-a-%d", ts)
	keyB := fmt.Sprintf("packev-b-%d", ts)

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(3*time.Minute))
	t.Cleanup(func() {
		_ = c.Shutdown(context.Background(), keyA)
		_ = c.Shutdown(context.Background(), keyB)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mk := func(key, allowHost string) *hiverclient.Sandbox {
		cfg := hiverclient.SandboxConfig{
			Image:      image,
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			FS: []hiverclient.FileSystem{
				{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/workspace/**", Access: "rw"}}},
			},
			Egress: []hiverclient.EgressRule{{Access: "allow", Host: allowHost}},
		}
		sbx, err := c.GetOrCreateSandbox(ctx, key, cfg)
		if err != nil {
			t.Fatalf("GetOrCreateSandbox(%s): %v", key, err)
		}
		return sbx
	}

	sbxA := mk(keyA, "example.com")
	sbxB := mk(keyB, "example.org")

	// Both sandboxes must land in the same pod for proxyRouter routing to be exercised.
	if sbxA.ID != sbxB.ID {
		t.Skipf("keys landed in different pods (%s vs %s) — controller not in pack mode (HIVE_PACK=1)", sbxA.ID, sbxB.ID)
	}
	t.Logf("both keys packed into pod %s", sbxA.ID)

	execSbx := func(sbx *hiverclient.Sandbox, sh string) hiverclient.ExecResult {
		r, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: []string{"sh", "-c", sh}})
		if err != nil {
			t.Fatalf("exec %q: %v", sh, err)
		}
		return *r
	}

	// Connect to the DNS-sink placeholder IP with a Host header so sbxproxy makes
	// an allow/deny decision based solely on the Host header and src IP (no real DNS).
	triggerRequest := func(sbx *hiverclient.Sandbox, host string) {
		py := fmt.Sprintf(
			"import http.client as h\n"+
				"try:\n"+
				"  c=h.HTTPConnection('192.0.2.1',80,timeout=15)\n"+
				"  c.request('GET','/',headers={'Host':'%s'})\n"+
				"  c.getresponse()\n"+
				"except Exception: pass",
			host)
		execSbx(sbx, "python3 -c \""+strings.ReplaceAll(py, "\"", "\\\"")+"\"")
	}

	// A → example.com (allowed for A), A → example.org (denied for A)
	triggerRequest(sbxA, "example.com")
	triggerRequest(sbxA, "example.org")
	// B → example.org (allowed for B), B → example.com (denied for B)
	triggerRequest(sbxB, "example.org")
	triggerRequest(sbxB, "example.com")

	// Collect each sandbox's event stream independently. Broker replays from id=0,
	// so all events above are visible even though they completed before this call.
	eventsA := collectSandboxEvents(t, sbxA, ctx, 5*time.Second)
	eventsB := collectSandboxEvents(t, sbxB, ctx, 5*time.Second)
	t.Logf("sandbox A: %d events, sandbox B: %d events", len(eventsA), len(eventsB))

	reqsA := filterByType(eventsA, "egress.request")
	reqsB := filterByType(eventsB, "egress.request")

	t.Run("A_allowed_example.com", func(t *testing.T) {
		if findEvent(reqsA, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "allowed" && e.Host == "example.com"
		}) == nil {
			t.Fatalf("sandbox A: no egress.request{allowed, example.com}; requests:\n%s", summarizeEgressReqs(reqsA))
		}
	})
	t.Run("A_denied_example.org", func(t *testing.T) {
		if findEvent(reqsA, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "denied" && e.Host == "example.org"
		}) == nil {
			t.Fatalf("sandbox A: no egress.request{denied, example.org}; requests:\n%s", summarizeEgressReqs(reqsA))
		}
	})
	t.Run("B_allowed_example.org", func(t *testing.T) {
		if findEvent(reqsB, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "allowed" && e.Host == "example.org"
		}) == nil {
			t.Fatalf("sandbox B: no egress.request{allowed, example.org}; requests:\n%s", summarizeEgressReqs(reqsB))
		}
	})
	t.Run("B_denied_example.com", func(t *testing.T) {
		if findEvent(reqsB, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "denied" && e.Host == "example.com"
		}) == nil {
			t.Fatalf("sandbox B: no egress.request{denied, example.com}; requests:\n%s", summarizeEgressReqs(reqsB))
		}
	})

	// Isolation: events routed by src_ip must not bleed across sandbox brokers.
	t.Run("A_no_crosscontamination", func(t *testing.T) {
		if findEvent(reqsA, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "allowed" && e.Host == "example.org"
		}) != nil {
			t.Errorf("sandbox A: found egress.request{allowed, example.org} — B's events leaked into A's stream")
		}
	})
	t.Run("B_no_crosscontamination", func(t *testing.T) {
		if findEvent(reqsB, func(e hiverclient.SandboxEvent) bool {
			return e.Access == "allowed" && e.Host == "example.com"
		}) != nil {
			t.Errorf("sandbox B: found egress.request{allowed, example.com} — A's events leaked into B's stream")
		}
	})
}
