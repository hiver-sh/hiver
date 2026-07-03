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
)

// TestFileE2E exercises WriteFile, ReadFile, and ListDirectory against a
// hiversh/python:3.13-alpine sandbox. It covers the upload/download round-trip,
// exec-side verification that uploads land on the FUSE mount, directory listing,
// binary content fidelity, and the error path for a missing file.
func TestFileE2E(t *testing.T) {
	setup.RequireStack(t)
	setup.RequireHiverCLI(t)

	key := fmt.Sprintf("e2e-file-%d", time.Now().UnixNano())
	config := hiverclient.SandboxConfig{
		Image:      "hiversh/python:3.13-alpine",
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		FS: []hiverclient.FileSystem{
			{Mount: "/workspace", Backend: "local", ACLs: []hiverclient.ACLRule{{Path: "/**", Access: "rw"}}},
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

	t.Run("upload_then_download", func(t *testing.T) {
		content := []byte("hello from upload")
		res, err := sbx.WriteFile(ctx, "/workspace/greeting.txt", content)
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if res.Path != "/workspace/greeting.txt" {
			t.Errorf("path=%q, want /workspace/greeting.txt", res.Path)
		}
		if res.Bytes != int64(len(content)) {
			t.Errorf("bytes=%d, want %d", res.Bytes, len(content))
		}

		got, err := sbx.ReadFile(ctx, "/workspace/greeting.txt")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("downloaded content=%q, want %q", got, content)
		}
	})

	t.Run("exec_verifies_upload", func(t *testing.T) {
		content := []byte("exec-check-content")
		if _, err := sbx.WriteFile(ctx, "/workspace/exec-check.txt", content); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: "cat /workspace/exec-check.txt"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("exit_code=%d, want 0", res.ExitCode)
		}
		if !strings.Contains(res.Stdout, "exec-check-content") {
			t.Errorf("stdout=%q, want 'exec-check-content'", res.Stdout)
		}
	})

	t.Run("list_directory", func(t *testing.T) {
		for _, name := range []string{"alpha.txt", "beta.txt"} {
			if _, err := sbx.WriteFile(ctx, "/workspace/"+name, []byte(name)); err != nil {
				t.Fatalf("WriteFile(%s): %v", name, err)
			}
		}
		entries, err := sbx.ListDirectory(ctx, "/workspace")
		if err != nil {
			t.Fatalf("ListDirectory: %v", err)
		}
		names := make(map[string]bool, len(entries))
		for _, e := range entries {
			names[e.Name] = true
		}
		for _, want := range []string{"alpha.txt", "beta.txt"} {
			if !names[want] {
				t.Errorf("ListDirectory: missing %q; got %v", want, names)
			}
		}
	})

	t.Run("list_directory_shows_subdir", func(t *testing.T) {
		res, err := sbx.Exec(ctx, hiverclient.ExecRequest{Command: "mkdir -p /workspace/subdir"})
		if err != nil {
			t.Fatalf("Exec mkdir: %v", err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("mkdir exit_code=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		entries, err := sbx.ListDirectory(ctx, "/workspace")
		if err != nil {
			t.Fatalf("ListDirectory: %v", err)
		}
		var found bool
		for _, e := range entries {
			if e.Name == "subdir" && e.IsDir {
				found = true
			}
		}
		if !found {
			t.Error("ListDirectory: subdir not found as a directory entry")
		}
	})

	t.Run("upload_binary_roundtrip", func(t *testing.T) {
		var content [256]byte
		for i := range content {
			content[i] = byte(i)
		}
		if _, err := sbx.WriteFile(ctx, "/workspace/binary.bin", content[:]); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := sbx.ReadFile(ctx, "/workspace/binary.bin")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, content[:]) {
			t.Errorf("binary content mismatch: got %d bytes, want %d", len(got), len(content))
		}
	})

	t.Run("download_missing_file_error", func(t *testing.T) {
		_, err := sbx.ReadFile(ctx, "/workspace/does-not-exist.bin")
		if err == nil {
			t.Error("ReadFile: expected error for missing file, got nil")
		}
	})

	t.Run("delete_file", func(t *testing.T) {
		content := []byte("to be deleted")
		if _, err := sbx.WriteFile(ctx, "/workspace/delete-me.txt", content); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := sbx.DeleteFile(ctx, "/workspace/delete-me.txt"); err != nil {
			t.Fatalf("DeleteFile: %v", err)
		}
		_, err := sbx.ReadFile(ctx, "/workspace/delete-me.txt")
		if err == nil {
			t.Error("ReadFile after delete: expected error, got nil")
		}
	})

	t.Run("delete_missing_file_error", func(t *testing.T) {
		err := sbx.DeleteFile(ctx, "/workspace/does-not-exist.txt")
		if err == nil {
			t.Error("DeleteFile: expected error for missing file, got nil")
		}
	})
}
