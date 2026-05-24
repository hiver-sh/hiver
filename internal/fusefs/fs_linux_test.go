//go:build linux

package fusefs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/blasten/hive/internal/fusefs"
)

// requiresFUSE skips a test when /dev/fuse isn't available (CI without
// privileged FUSE access, or non-Linux). bazil/fuse will fail at Mount
// time, which is enough for an early skip.
func requiresFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("FUSE not available: %v", err)
	}
}

func startFUSE(t *testing.T, rules []fusefs.Rule) (mountPoint, backend string, audit *bytes.Buffer, stop func()) {
	t.Helper()
	requiresFUSE(t)
	backend = t.TempDir()
	mountPoint = t.TempDir()
	audit = &bytes.Buffer{}
	// Tests express rules in mount-relative form ("/", "/secret/**")
	// for readability; the evaluator now sees absolute paths, so we
	// prefix the dynamic mountPoint onto each rule before compiling.
	abs := make([]fusefs.Rule, len(rules))
	for i, r := range rules {
		abs[i] = fusefs.Rule{
			Path:   path.Clean(mountPoint + "/" + r.Path),
			Access: r.Access,
		}
	}
	srv, err := fusefs.Mount(fusefs.Config{
		MountPoint: mountPoint,
		Backend:    backend,
		ACLs:       fusefs.Compile(abs),
		Audit:      audit,
	})
	if err != nil {
		t.Skipf("fusefs.Mount: %v (FUSE may not be available in this environment)", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	// Wait for mount to register.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mountPoint); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	stop = func() {
		cancel()
		_ = srv.Unmount()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	return mountPoint, backend, audit, stop
}

func decodeFSEvents(t *testing.T, b *bytes.Buffer) []fusefs.AuditEvent {
	t.Helper()
	var out []fusefs.AuditEvent
	dec := json.NewDecoder(b)
	for {
		var e fusefs.AuditEvent
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode audit: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func hasOp(events []fusefs.AuditEvent, op, verdict string) bool {
	for _, e := range events {
		if e.Op == op && e.Verdict == verdict {
			return true
		}
	}
	return false
}

func TestFUSEReadWriteRoundTrip(t *testing.T) {
	mp, backend, audit, stop := startFUSE(t, []fusefs.Rule{
		{Path: "/", Access: fusefs.AccessRW},
		{Path: "/**", Access: fusefs.AccessRW},
	})
	defer stop()
	_ = backend

	// Write through the mount.
	if err := os.WriteFile(filepath.Join(mp, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write through mount: %v", err)
	}
	// Read it back.
	data, err := os.ReadFile(filepath.Join(mp, "hello.txt"))
	if err != nil {
		t.Fatalf("read through mount: %v", err)
	}
	if string(data) != "hi" {
		t.Errorf("contents: got %q, want %q", data, "hi")
	}

	events := decodeFSEvents(t, audit)
	if !hasOp(events, "create", "allow") {
		t.Error("expected a create-allow audit event")
	}
	if !hasOp(events, "write", "allow") {
		t.Error("expected a write-allow audit event")
	}
}

func TestFUSEDenyReturnsENOENT(t *testing.T) {
	mp, backend, audit, stop := startFUSE(t, []fusefs.Rule{
		{Path: "/", Access: fusefs.AccessRW},
		{Path: "/**", Access: fusefs.AccessRW},
		{Path: "/secret/**", Access: fusefs.AccessDeny},
	})
	defer stop()

	// Create a file in the deny-tree directly on the backend (bypassing FUSE),
	// so it physically exists. The agent should still see ENOENT through FUSE.
	if err := os.MkdirAll(filepath.Join(backend, "secret"), 0o755); err != nil {
		t.Fatalf("mkdir backend: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backend, "secret", "keys.txt"), []byte("ssshh"), 0o600); err != nil {
		t.Fatalf("write backend: %v", err)
	}

	_, err := os.Stat(filepath.Join(mp, "secret", "keys.txt"))
	if err == nil {
		t.Fatal("expected error on denied path; got nil")
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || !errors.Is(pathErr.Err, syscall.ENOENT) {
		t.Errorf("expected ENOENT; got %v", err)
	}

	events := decodeFSEvents(t, audit)
	if !hasOp(events, "lookup", "deny") {
		t.Errorf("expected a lookup-deny audit event; got %+v", events)
	}
}

func TestFUSEReadOnlyRejectsWrites(t *testing.T) {
	mp, backend, audit, stop := startFUSE(t, []fusefs.Rule{
		{Path: "/", Access: fusefs.AccessRW},
		{Path: "/**", Access: fusefs.AccessRW},
		{Path: "/etc/**", Access: fusefs.AccessRO},
	})
	defer stop()

	if err := os.MkdirAll(filepath.Join(backend, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backend, "etc", "config"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// Reading should succeed.
	if _, err := os.ReadFile(filepath.Join(mp, "etc", "config")); err != nil {
		t.Errorf("read of ro file failed: %v", err)
	}

	// Writing should fail.
	err := os.WriteFile(filepath.Join(mp, "etc", "config"), []byte("nope"), 0o644)
	if err == nil {
		t.Error("expected error writing to ro path; got nil")
	}

	events := decodeFSEvents(t, audit)
	if !hasOp(events, "open-write", "deny") && !hasOp(events, "write", "deny") {
		t.Errorf("expected an open-write-deny or write-deny audit event; got %+v", events)
	}
}

func TestFUSESymlink(t *testing.T) {
	mp, _, _, stop := startFUSE(t, []fusefs.Rule{
		{Path: "/", Access: fusefs.AccessRW},
		{Path: "/**", Access: fusefs.AccessRW},
	})
	defer stop()

	linkPath := filepath.Join(mp, "lib")
	target := "lib64"
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	got, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != target {
		t.Fatalf("Readlink = %q, want %q", got, target)
	}
	// Confirm it appears in directory listing as a symlink.
	entries, err := os.ReadDir(filepath.Dir(linkPath))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "lib" {
			found = true
			if e.Type()&os.ModeSymlink == 0 {
				t.Errorf("entry type = %v, want symlink", e.Type())
			}
		}
	}
	if !found {
		t.Error("symlink not found in directory listing")
	}
}

// TestFUSERemoveNonEmptyDirReturnsENOTEMPTY guards against a regression where
// mapErr returned *os.PathError instead of syscall.Errno, causing bazil.org/fuse
// to substitute EIO for ENOTEMPTY. npm staging directories hit this when
// rmdir is called before the directory is fully emptied.
func TestFUSERemoveNonEmptyDirReturnsENOTEMPTY(t *testing.T) {
	mp, _, _, stop := startFUSE(t, []fusefs.Rule{
		{Path: "/", Access: fusefs.AccessRW},
		{Path: "/**", Access: fusefs.AccessRW},
	})
	defer stop()

	dir := filepath.Join(mp, "staging")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	err := syscall.Rmdir(dir)
	if err == nil {
		t.Fatal("expected error removing non-empty directory; got nil")
	}
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Errorf("got %v (%T), want ENOTEMPTY", err, err)
	}
}

// TestFUSEMkdirExistingReturnsEEXIST guards against mapErr returning *os.PathError
// (which bazil maps to EIO) instead of EEXIST when mkdir is called on a path
// that already exists.
func TestFUSEMkdirExistingReturnsEEXIST(t *testing.T) {
	mp, _, _, stop := startFUSE(t, []fusefs.Rule{
		{Path: "/", Access: fusefs.AccessRW},
		{Path: "/**", Access: fusefs.AccessRW},
	})
	defer stop()

	dir := filepath.Join(mp, "existing")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir first: %v", err)
	}

	err := syscall.Mkdir(dir, 0o755)
	if err == nil {
		t.Fatal("expected error on duplicate mkdir; got nil")
	}
	if !errors.Is(err, syscall.EEXIST) {
		t.Errorf("got %v (%T), want EEXIST", err, err)
	}
}

// TestFUSERenameNonExistentReturnsENOENT guards against mapErr returning
// *os.PathError (EIO) instead of ENOENT when the rename source doesn't exist.
func TestFUSERenameNonExistentReturnsENOENT(t *testing.T) {
	mp, _, _, stop := startFUSE(t, []fusefs.Rule{
		{Path: "/", Access: fusefs.AccessRW},
		{Path: "/**", Access: fusefs.AccessRW},
	})
	defer stop()

	err := os.Rename(filepath.Join(mp, "ghost"), filepath.Join(mp, "dst"))
	if err == nil {
		t.Fatal("expected error renaming non-existent source; got nil")
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || !errors.Is(pathErr.Err, syscall.ENOENT) {
		t.Errorf("got %v (%T), want ENOENT", err, err)
	}
}

func TestFUSEDeniedDirEntriesHidden(t *testing.T) {
	mp, backend, _, stop := startFUSE(t, []fusefs.Rule{
		{Path: "/", Access: fusefs.AccessRW},
		{Path: "/**", Access: fusefs.AccessRW},
		{Path: "/hidden", Access: fusefs.AccessDeny},
	})
	defer stop()

	// Create both a visible and a hidden entry on the backend.
	if err := os.WriteFile(filepath.Join(backend, "visible.txt"), []byte("v"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backend, "hidden"), []byte("h"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	entries, err := os.ReadDir(mp)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "hidden" {
			t.Errorf("denied entry %q leaked into directory listing", e.Name())
		}
	}
}
