package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestFSACLE2E verifies that sbxfuse picks up ACL changes on SIGHUP
// without restarting the mount. A deny rule is added to /scratch/locked/**,
// confirmed to block writes, then removed and confirmed to lift the block.
func TestFSACLE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	bundleImage := setup.BuildTSMCPServerBundle(t)
	key := fmt.Sprintf("e2e-fs-%d", time.Now().UnixNano())

	config := hiverclient.SandboxConfig{
		Image: bundleImage,
		FS: []hiverclient.FileSystem{
			{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
			{Mount: "/scratch", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
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

	session := setup.ConnectMCP(t, ctx, sbx.ProxyURL(3000)+"/mcp", &bytes.Buffer{})
	defer session.Close()

	initial, err := sbx.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	assertMountPresent(t, *initial, "/workspace", "local")
	assertMountPresent(t, *initial, "/scratch", "local")

	// /scratch must be fully writable before reconcile.
	if exit := agentBashExit(t, ctx, session, "echo before > /scratch/precheck.txt"); exit != 0 {
		t.Fatalf("/scratch must be writable before fs reconcile (exit=%d)", exit)
	}

	// Add a deny rule for /scratch/locked/**. sbxfuse picks it up on SIGHUP
	// and starts rejecting writes under that subtree without restarting the mount.
	withDeny := withScratchLockedDeny(*initial)
	fsApply, err := sbx.ApplyConfig(ctx, withDeny)
	if err != nil {
		t.Fatalf("fs PUT: %v", err)
	}
	if !fsApply.Applied {
		t.Fatalf("fs PUT: applied=false (error=%q)", fsApply.Error)
	}
	if fsApply.Changes.FS == nil {
		t.Fatalf("fs PUT: changes.fs is nil; want a diff: %+v", fsApply.Changes)
	}

	// fusefs returns ENOENT on deny, so touch exits non-zero.
	eventuallyAgentBashFails(t, ctx, session,
		"mkdir -p /scratch/locked && touch /scratch/locked/x.txt",
		5*time.Second)

	// Writes outside the denied subtree continue to succeed.
	if exit := agentBashExit(t, ctx, session, "echo still-ok > /scratch/sibling.txt"); exit != 0 {
		t.Fatalf("/scratch/sibling.txt write should still succeed (exit=%d)", exit)
	}

	// Restore original ACLs → deny lifts → writes under /scratch/locked succeed again.
	if r, err := sbx.ApplyConfig(ctx, *initial); err != nil {
		t.Fatalf("fs restore PUT: %v", err)
	} else if !r.Applied {
		t.Fatalf("fs restore PUT: applied=false (error=%q)", r.Error)
	}
	eventuallyAgentBashSucceeds(t, ctx, session,
		"mkdir -p /scratch/locked && touch /scratch/locked/y.txt",
		5*time.Second)
}

func assertMountPresent(t *testing.T, cfg hiverclient.SandboxConfig, mount, backend string) {
	t.Helper()
	for _, f := range cfg.FS {
		if f.Mount == mount {
			if f.Backend != backend {
				t.Errorf("mount %q: backend=%q, want %q", mount, f.Backend, backend)
			}
			return
		}
	}
	t.Errorf("mount %q not present in fs (got %+v)", mount, cfg.FS)
}

func withScratchLockedDeny(cfg hiverclient.SandboxConfig) hiverclient.SandboxConfig {
	out := cfg
	out.FS = make([]hiverclient.FileSystem, len(cfg.FS))
	copy(out.FS, cfg.FS)
	for i := range out.FS {
		if out.FS[i].Mount != "/scratch" {
			continue
		}
		out.FS[i].ACLs = append(
			append([]hiverclient.ACLRule(nil), out.FS[i].ACLs...),
			hiverclient.ACLRule{Path: "/scratch/locked/**", Access: "deny"},
		)
		break
	}
	return out
}

func agentBashExit(t *testing.T, ctx context.Context, session *mcp.ClientSession, cmd string) int {
	t.Helper()
	var out struct {
		Stdout, Stderr string
		ExitCode       int
	}
	setup.CallMCP(t, ctx, session, "bash", map[string]any{"cmd": cmd}, &out)
	return out.ExitCode
}

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
