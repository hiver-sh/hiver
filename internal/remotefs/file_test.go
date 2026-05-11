package remotefs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"testing"

	"github.com/sandbox-platform/agent-sandbox/internal/remotefs"
)

// TestFileStoreRoundTrip exercises the full Store contract against the
// FileStore impl: put, list (incl. nested), get, move, delete.
func TestFileStoreRoundTrip(t *testing.T) {
	s, err := remotefs.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	// Put two objects, one nested.
	if err := s.Put(ctx, "/foo.txt", bytes.NewBufferString("hello")); err != nil {
		t.Fatalf("Put /foo.txt: %v", err)
	}
	if err := s.Put(ctx, "/dir/bar.txt", bytes.NewBufferString("world")); err != nil {
		t.Fatalf("Put /dir/bar.txt: %v", err)
	}

	// List should see both.
	paths, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(paths)
	want := []string{"/dir/bar.txt", "/foo.txt"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Errorf("List: got %v, want %v", paths, want)
	}

	// Get round-trips content.
	rc, err := s.Get(ctx, "/foo.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "hello" {
		t.Errorf("Get content: got %q, want %q", body, "hello")
	}

	// Get on a missing path returns ErrNotExist.
	if _, err := s.Get(ctx, "/missing.txt"); !errors.Is(err, remotefs.ErrNotExist) {
		t.Errorf("Get missing: got %v, want ErrNotExist", err)
	}

	// Move relocates the object.
	if err := s.Move(ctx, "/foo.txt", "/renamed/foo.txt"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := s.Get(ctx, "/foo.txt"); !errors.Is(err, remotefs.ErrNotExist) {
		t.Errorf("after Move: source still exists, got err=%v", err)
	}
	rc, err = s.Get(ctx, "/renamed/foo.txt")
	if err != nil {
		t.Fatalf("Get moved: %v", err)
	}
	rc.Close()

	// Delete removes; second Delete is a no-op.
	if err := s.Delete(ctx, "/dir/bar.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "/dir/bar.txt"); err != nil {
		t.Errorf("idempotent Delete: %v", err)
	}
}
