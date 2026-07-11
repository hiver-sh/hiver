package e2e_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	hiverclient "github.com/hiver-sh/hiver/client"
	"github.com/hiver-sh/hiver/test/e2e/setup"
)

// TestFSGCSE2E exercises the GCS filesystem backend end-to-end via exec:
// multiple file writes and reads, deletes, renames (same-dir and cross-dir),
// nested directories, symlinks, and directory listing.
//
// Required environment variables:
//
//	HIVE_TEST_GCS_BUCKET  – the GCS bucket name
//	HIVE_TEST_GCS_SA_JSON - the service account
//
// Optional:
//
//	HIVE_TEST_GCS_PREFIX  – key prefix inside the bucket (default: "e2e-test")
func TestFSGCSE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	bucket := os.Getenv("HIVE_TEST_GCS_BUCKET")
	saJSON := os.Getenv("HIVE_TEST_GCS_SA_JSON")

	if bucket == "" {
		t.Skip("HIVE_TEST_GCS_BUCKET must be set")
	}

	prefix := strings.TrimSuffix(os.Getenv("HIVE_TEST_GCS_PREFIX"), "/")
	if prefix == "" {
		prefix = "e2e-test"
	}
	prefix = fmt.Sprintf("%s/%d", prefix, time.Now().UnixNano())

	key := fmt.Sprintf("e2e-fs-gcs-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "python",
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		FS: []hiverclient.FileSystem{{
			Mount:                 "/workspace",
			Backend:               "gcs",
			ACLs:                  []hiverclient.ACLRule{{Path: "/**", Access: "rw"}},
			GCSBucket:             bucket,
			GCSPrefix:             prefix,
			GCSServiceAccountJSON: saJSON,
		}},
	}

	c := hiverclient.NewClient(setup.GatewayURL, hiverclient.WithTimeout(2*time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sbx, err := c.GetOrCreateSandbox(ctx, key, config)
	if err != nil {
		t.Fatalf("GetOrCreateSandbox: %v", err)
	}
	// Tear the sandbox down via its own API (no controller involvement).
	t.Cleanup(func() { _ = sbx.Shutdown(context.Background()) })

	// exec runs a command inside the sandbox, fails the test on any error or
	// non-zero exit code, and returns trimmed stdout.
	exec := func(t *testing.T, cmd string) string {
		t.Helper()
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: cmd})
		if err != nil {
			t.Fatalf("Exec(%q): %v", cmd, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("Exec(%q) exit %d; stderr=%q", cmd, res.ExitCode, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}

	// fileExists returns whether a path exists inside the sandbox.
	fileExists := func(t *testing.T, path string) bool {
		t.Helper()
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: "test -e " + path})
		if err != nil {
			t.Fatalf("Exec(test -e %s): %v", path, err)
		}
		return res.ExitCode == 0
	}

	t.Run("write_multiple_files", func(t *testing.T) {
		files := []struct{ name, content string }{
			{"alpha.txt", "content-alpha"},
			{"beta.txt", "content-beta"},
			{"gamma.txt", "content-gamma"},
		}
		for _, f := range files {
			exec(t, fmt.Sprintf(`python3 -c "open('/workspace/%s', 'w').write('%s')"`, f.name, f.content))
		}
		for _, f := range files {
			got := exec(t, "cat /workspace/"+f.name)
			if got != f.content {
				t.Errorf("%s: read back %q, want %q", f.name, got, f.content)
			}
		}
	})

	t.Run("overwrite_file", func(t *testing.T) {
		exec(t, `python3 -c "open('/workspace/overwrite.txt', 'w').write('v1')"`)
		exec(t, `python3 -c "open('/workspace/overwrite.txt', 'w').write('v2')"`)
		got := exec(t, "cat /workspace/overwrite.txt")
		if got != "v2" {
			t.Errorf("overwrite: got %q, want 'v2'", got)
		}
	})

	t.Run("nested_directory", func(t *testing.T) {
		exec(t, "mkdir -p /workspace/subdir/nested")
		exec(t, `python3 -c "open('/workspace/subdir/nested/deep.txt', 'w').write('deep-content')"`)

		got := exec(t, "cat /workspace/subdir/nested/deep.txt")
		if got != "deep-content" {
			t.Errorf("nested file: got %q, want 'deep-content'", got)
		}
		if !fileExists(t, "/workspace/subdir") {
			t.Error("subdir not visible in FUSE")
		}
	})

	t.Run("delete_file", func(t *testing.T) {
		exec(t, `python3 -c "open('/workspace/to-delete.txt', 'w').write('ephemeral')"`)
		if !fileExists(t, "/workspace/to-delete.txt") {
			t.Fatal("file not present before delete")
		}

		exec(t, "rm /workspace/to-delete.txt")

		if fileExists(t, "/workspace/to-delete.txt") {
			t.Error("file still present after rm")
		}
	})

	t.Run("rename_file", func(t *testing.T) {
		exec(t, `python3 -c "open('/workspace/before.txt', 'w').write('will-be-renamed')"`)
		exec(t, "mv /workspace/before.txt /workspace/after.txt")

		if fileExists(t, "/workspace/before.txt") {
			t.Error("old path still present after mv")
		}
		got := exec(t, "cat /workspace/after.txt")
		if got != "will-be-renamed" {
			t.Errorf("renamed file: got %q, want 'will-be-renamed'", got)
		}
	})

	t.Run("rename_across_dirs", func(t *testing.T) {
		exec(t, "mkdir -p /workspace/src /workspace/dst")
		exec(t, `python3 -c "open('/workspace/src/move-me.txt', 'w').write('cross-dir')"`)
		exec(t, "mv /workspace/src/move-me.txt /workspace/dst/moved.txt")

		if fileExists(t, "/workspace/src/move-me.txt") {
			t.Error("src still present after cross-dir mv")
		}
		got := exec(t, "cat /workspace/dst/moved.txt")
		if got != "cross-dir" {
			t.Errorf("cross-dir rename: got %q, want 'cross-dir'", got)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		exec(t, `python3 -c "open('/workspace/real.txt', 'w').write('symlink-target')"`)
		exec(t, "ln -s /workspace/real.txt /workspace/link.txt")

		got := exec(t, "cat /workspace/link.txt")
		if got != "symlink-target" {
			t.Errorf("read through symlink: got %q, want 'symlink-target'", got)
		}
		target := exec(t, "readlink /workspace/link.txt")
		if target != "/workspace/real.txt" {
			t.Errorf("readlink: got %q, want '/workspace/real.txt'", target)
		}
	})

	t.Run("delete_then_recreate", func(t *testing.T) {
		exec(t, `python3 -c "open('/workspace/recycled.txt', 'w').write('first')"`)
		exec(t, "rm /workspace/recycled.txt")
		exec(t, `python3 -c "open('/workspace/recycled.txt', 'w').write('second')"`)

		got := exec(t, "cat /workspace/recycled.txt")
		if got != "second" {
			t.Errorf("recreated file: got %q, want 'second'", got)
		}
	})

	t.Run("list_dir", func(t *testing.T) {
		entries, err := sbx.ListDirectory(ctx, "/workspace")
		if err != nil {
			t.Fatalf("ListDirectory: %v", err)
		}
		names := make(map[string]bool, len(entries))
		for _, e := range entries {
			names[e.Name] = true
		}
		for _, want := range []string{"alpha.txt", "beta.txt", "gamma.txt", "subdir"} {
			if !names[want] {
				t.Errorf("ListDirectory: missing %q; got %v", want, names)
			}
		}
		// after.txt and dst/ were created by rename subtests
		for _, want := range []string{"after.txt", "dst"} {
			if !names[want] {
				t.Errorf("ListDirectory: missing %q (from rename tests); got %v", want, names)
			}
		}
	})

	t.Run("fs_response_backend_gcs", func(t *testing.T) {
		events := collectSandboxEvents(t, sbx, ctx, 5*time.Second)
		fsResps := filterByType(events, "fs.response")
		ev := findEvent(fsResps, func(e hiverclient.SandboxEvent) bool {
			return e.Backend == "gcs"
		})
		if ev == nil {
			t.Fatalf("no fs.response with backend=gcs; responses:\n%s", summarizeFSResps(fsResps))
		}
	})
}
