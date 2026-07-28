//go:build linux

package fusefs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiver-sh/hiver/internal/remotefs"
)

// TestBootstrapPullsRemoteIntoBuffer covers the async-mount background pull:
// every remote object lands in the local buffer (so reads can be served
// locally), a nested path creates its parent directory, and a path that
// already exists locally (an agent write) is never clobbered by the older
// remote copy. bootstrap needs no FUSE mount — it only walks the store and
// writes the backend dir — so this runs as a plain unit test.
func TestBootstrapPullsRemoteIntoBuffer(t *testing.T) {
	ctx := context.Background()
	remoteDir := t.TempDir()
	backend := t.TempDir()

	store, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Put(ctx, "/a.txt", strings.NewReader("alpha")); err != nil {
		t.Fatalf("seed a.txt: %v", err)
	}
	if err := store.Put(ctx, "/dir/b.txt", strings.NewReader("bravo")); err != nil {
		t.Fatalf("seed dir/b.txt: %v", err)
	}
	// A file the agent already wrote locally must survive the pull.
	if err := os.WriteFile(filepath.Join(backend, "a.txt"), []byte("LOCAL"), 0o644); err != nil {
		t.Fatalf("seed local a.txt: %v", err)
	}

	s := &Server{cfg: Config{MountPoint: "/workspace", Backend: backend, Remote: store, Async: true}}
	s.bootstrap(ctx)

	// Nested remote-only file pulled, parent dir created.
	if got, err := os.ReadFile(filepath.Join(backend, "dir", "b.txt")); err != nil || string(got) != "bravo" {
		t.Errorf("dir/b.txt = %q, err=%v; want bravo", got, err)
	}
	// Pre-existing local file left intact (not overwritten by remote "alpha").
	if got, err := os.ReadFile(filepath.Join(backend, "a.txt")); err != nil || string(got) != "LOCAL" {
		t.Errorf("a.txt = %q, err=%v; want LOCAL (must not clobber agent write)", got, err)
	}
}

// TestBootstrapOneSkipsAndFetches pins bootstrapOne's per-path decisions:
// an already-local path is skipped, a remote-only path is fetched.
func TestBootstrapOneSkipsAndFetches(t *testing.T) {
	ctx := context.Background()
	remoteDir := t.TempDir()
	backend := t.TempDir()

	store, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Put(ctx, "/only-remote.txt", strings.NewReader("remote")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &Server{cfg: Config{MountPoint: "/workspace", Backend: backend, Remote: store, Async: true}}

	// Remote-only path: fetched into the buffer.
	if err := s.bootstrapOne(ctx, "/only-remote.txt"); err != nil {
		t.Fatalf("bootstrapOne fetch: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(backend, "only-remote.txt")); string(got) != "remote" {
		t.Errorf("only-remote.txt = %q, want remote", got)
	}

	// A path absent from the remote is a no-op, not an error.
	if err := s.bootstrapOne(ctx, "/nonexistent.txt"); err != nil {
		t.Errorf("bootstrapOne on absent path: unexpected err %v", err)
	}
	if _, err := os.Lstat(filepath.Join(backend, "nonexistent.txt")); !os.IsNotExist(err) {
		t.Errorf("nonexistent.txt should not have been created")
	}
}
