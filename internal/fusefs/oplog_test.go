//go:build linux

package fusefs_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sandbox-platform/agent-sandbox/internal/fusefs"
	"github.com/sandbox-platform/agent-sandbox/internal/remotefs"
)

// TestOplogReplaysFsMutations writes / renames / removes through a
// FUSE mount whose Config carries an Oplog targeting a [remotefs.Store].
// We assert the mutations replicate to the store with the same paths
// the agent used. This is the prototype's stand-in for "google-drive
// backend" — the same Oplog flow, with a Drive client behind the
// Store interface, would prove out the real backend.
func TestOplogReplaysFsMutations(t *testing.T) {
	requiresFUSE(t)

	mountPoint := t.TempDir()
	backend := t.TempDir()
	remoteDir := t.TempDir()
	auditBuf := &bytes.Buffer{}

	store, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	oplog := fusefs.NewOplog(store, 16)

	// Allow rw everywhere for the test.
	rules := []fusefs.Rule{
		{Path: filepath.Clean(mountPoint), Access: fusefs.AccessRW},
		{Path: filepath.Clean(mountPoint) + "/**", Access: fusefs.AccessRW},
	}

	srv, err := fusefs.Mount(fusefs.Config{
		MountPoint: mountPoint,
		Backend:    backend,
		ACLs:       fusefs.Compile(rules),
		Audit:      auditBuf,
		Oplog:      oplog,
	})
	if err != nil {
		t.Skipf("fusefs.Mount: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	defer func() {
		cancel()
		_ = srv.Unmount()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}()

	// Wait for mount to register.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mountPoint); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Drive the oplog in lock-step so the test isn't time-flaky: write
	// a file, advance the queue, then assert. Calling flush via Run +
	// short timeouts simulates the production async path closely.
	go oplog.Run(ctx)

	// 1. Create + write → expect Put on the store.
	src := filepath.Join(mountPoint, "hello.txt")
	if err := os.WriteFile(src, []byte("hi from agent"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForRemote(t, store, filepath.Clean(mountPoint+"/hello.txt"), "hi from agent")

	// 2. Rename → expect Move on the store.
	dst := filepath.Join(mountPoint, "renamed.txt")
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}
	waitForRemote(t, store, filepath.Clean(mountPoint+"/renamed.txt"), "hi from agent")
	waitForAbsent(t, store, filepath.Clean(mountPoint+"/hello.txt"))

	// 3. Remove → expect Delete on the store.
	if err := os.Remove(dst); err != nil {
		t.Fatalf("remove: %v", err)
	}
	waitForAbsent(t, store, filepath.Clean(mountPoint+"/renamed.txt"))
}

// waitForRemote polls the store until path appears with the expected
// content, or fails the test after a short timeout. Uses polling
// because the oplog drains asynchronously.
func waitForRemote(t *testing.T, store remotefs.Store, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rc, err := store.Get(context.Background(), path)
		if err == nil {
			body := make([]byte, 0, len(want)+8)
			buf := make([]byte, 1024)
			for {
				n, rerr := rc.Read(buf)
				body = append(body, buf[:n]...)
				if rerr != nil {
					break
				}
			}
			rc.Close()
			if string(body) == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("remote: %q never reached content %q", path, want)
}

// waitForAbsent polls the store until path is gone.
func waitForAbsent(t *testing.T, store remotefs.Store, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rc, err := store.Get(context.Background(), path)
		if err == remotefs.ErrNotExist {
			return
		}
		if rc != nil {
			rc.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("remote: %q never disappeared", path)
}
