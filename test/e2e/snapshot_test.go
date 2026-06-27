package e2e_test

import (
	"context"
	"fmt"
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

	// ── Phase 1: write snapshots ─────────────────────────────────────────────
	// Start each sandbox, write a unique file, then shut it down so sandboxd
	// captures the /workspace contents into a snapshot tarball.
	// Shutdown blocks until the container exits, guaranteeing the tarball is
	// on disk before the restore phase begins.
	for _, tc := range cases {
		wKey := "w" + tc.key
		t.Cleanup(func() { _ = c.Shutdown(context.Background(), wKey) })

		sbx, err := c.GetOrCreateSandbox(ctx, wKey, hiverclient.SandboxConfig{
			Image:      "hiversh/python:3.13-alpine",
			Entrypoint: []string{"tail", "-f", "/dev/null"},
			Snapshot:   &hiverclient.Snapshot{Files: &hiverclient.SnapshotFiles{Key: tc.key, WriteOnShutdown: true}},
		})
		if err != nil {
			t.Fatalf("[%s] write: GetOrCreateSandbox: %v", tc.name, err)
		}

		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{
			Command: fmt.Sprintf("echo %s > %s", tc.content, tc.file),
		})
		if err != nil {
			t.Fatalf("[%s] write: Exec: %v", tc.name, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("[%s] write: echo failed (exit=%d stderr=%q)", tc.name, res.ExitCode, res.Stderr)
		}

		if err := c.Shutdown(ctx, wKey); err != nil {
			t.Fatalf("[%s] write: Shutdown: %v", tc.name, err)
		}
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
			t.Cleanup(func() { _ = c.Shutdown(context.Background(), rKey) })

			sbx, err := c.GetOrCreateSandbox(ctx, rKey, hiverclient.SandboxConfig{
				Image:      "hiversh/python:3.13-alpine",
				Entrypoint: []string{"tail", "-f", "/dev/null"},
				Snapshot:   &hiverclient.Snapshot{Files: &hiverclient.SnapshotFiles{Key: tc.key}},
			})
			if err != nil {
				t.Fatalf("GetOrCreateSandbox: %v", err)
			}

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
