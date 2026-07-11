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

// TestSnapshotE2E verifies the snapshot write/restore lifecycle end-to-end.
//
// Two independent key pairs ("alpha" and "beta") are exercised with the same
// structure:
//
//  1. A sandbox with Snapshot.Files (write_on_shutdown) writes a unique file to
//     /workspace, then is shut down. sandboxd captures the workspace into a
//     tarball before exiting; Shutdown blocks until the container exits, so the
//     tarball is guaranteed to be present before the restore phase.
//
//  2. A fresh sandbox with the same Snapshot.Files.Key (pointing to the tarball
//     from step 1) starts and reads the file back — verifying round-trip fidelity.
//
// Additionally, each restore sandbox asserts that files from the other key's
// snapshot are absent, proving that snapshot keys are isolated from each other.
func TestSnapshotE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	ts := time.Now().UnixNano()

	type snapshotCase struct {
		name    string // subtest label
		key     string // snapshot key used for both write and restore
		file    string // absolute path inside the sandbox
		content string // content written and verified
	}

	cases := []snapshotCase{
		{
			name:    "alpha",
			key:     fmt.Sprintf("sna%d", ts),
			file:    "/workspace/alpha.txt",
			content: "hello-alpha",
		},
		{
			name:    "beta",
			key:     fmt.Sprintf("snb%d", ts),
			file:    "/workspace/beta.txt",
			content: "hello-beta",
		},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// waitSnapshot blocks until the local snapshot tarball for key lands on the
	// host-side snapshot dir. Shutdown is asynchronous (the teardown goroutine
	// captures /workspace after DELETE returns), so the restore phase must wait
	// for the tarball rather than assume it is already on disk.
	waitSnapshot := func(key string) {
		t.Helper()
		path := filepath.Join(os.Getenv("HOME"), ".hive", "snapshots", "snapshot-"+key+".tar.gz")
		deadline := time.Now().Add(30 * time.Second)
		for {
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("snapshot for key %q not found at %s after 30s", key, path)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// ── Phase 1: write snapshots ─────────────────────────────────────────────
	// Start each sandbox, write a unique file, then shut it down so sandboxd
	// captures the /workspace contents into a snapshot tarball, and wait for that
	// tarball before moving on to the restore phase.
	for _, tc := range cases {
		wKey := "w" + tc.key

		sbx, err := c.GetOrCreateSandbox(ctx, wKey, hiverclient.SandboxConfig{
			Image:      "python",
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			// Include /workspace explicitly: an empty include captures only the
			// container's overlay upper, not FUSE-mounted paths like /workspace,
			// so the files written below would otherwise be left out of the tarball.
			Snapshot: &hiverclient.Snapshot{Files: &hiverclient.SnapshotFiles{
				Key:             tc.key,
				WriteOnShutdown: true,
				Include:         []string{"/workspace/**"},
			}},
		})
		if err != nil {
			t.Fatalf("[%s] write: GetOrCreateSandbox: %v", tc.name, err)
		}
		// Tear the sandbox down via its own API (no controller involvement).
		t.Cleanup(func() { _ = sbx.Shutdown(context.Background()) })

		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
			Command: fmt.Sprintf("echo %s > %s", tc.content, tc.file),
		})
		if err != nil {
			t.Fatalf("[%s] write: Exec: %v", tc.name, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("[%s] write: echo failed (exit=%d stderr=%q)", tc.name, res.ExitCode, res.Stderr)
		}

		if err := sbx.Shutdown(ctx); err != nil {
			t.Fatalf("[%s] write: Shutdown: %v", tc.name, err)
		}
		waitSnapshot(tc.key)
	}

	// ── Phase 2: restore snapshots ───────────────────────────────────────────
	// For each case, start a fresh sandbox that restores from the key written
	// above, then assert:
	//   (a) the file is present with the correct content
	//   (b) files from other snapshot keys are absent (isolation)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rKey := "r" + tc.key

			sbx, err := c.GetOrCreateSandbox(ctx, rKey, hiverclient.SandboxConfig{
				Image:      "python",
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				Snapshot:   &hiverclient.Snapshot{Files: &hiverclient.SnapshotFiles{Key: tc.key}},
			})
			if err != nil {
				t.Fatalf("GetOrCreateSandbox: %v", err)
			}
			// Tear the sandbox down via its own API (no controller involvement).
			t.Cleanup(func() { _ = sbx.Shutdown(context.Background()) })

			// (a) The written file must be restored with the correct content.
			res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
				Command: fmt.Sprintf("cat %s", tc.file),
			})
			if err != nil {
				t.Fatalf("Exec cat: %v", err)
			}
			if res.ExitCode != 0 {
				t.Fatalf("cat %s: exit=%d stderr=%q", tc.file, res.ExitCode, res.Stderr)
			}
			if got := strings.TrimSpace(res.Stdout); got != tc.content {
				t.Errorf("restored content: got %q, want %q", got, tc.content)
			}

			// (b) Files that belong to other snapshot keys must not appear —
			// each key produces an independent snapshot tarball.
			for _, other := range cases {
				if other.key == tc.key {
					continue
				}
				res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
					Command: fmt.Sprintf("test ! -f %s", other.file),
				})
				if err != nil {
					t.Fatalf("Exec isolation check: %v", err)
				}
				if res.ExitCode != 0 {
					t.Errorf("isolation: %s must not exist when restoring key %s", other.file, tc.key)
				}
			}
		})
	}
}
