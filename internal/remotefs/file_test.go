package remotefs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"testing"

	"github.com/hiver-sh/hiver/internal/remotefs"
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

// TestFileStoreSymlink pins the symlink contract on the native-FS backend:
// Symlink creates it, Stat/ListDir report it with the Symlink bit + target,
// and Readlink returns the target.
func TestFileStoreSymlink(t *testing.T) {
	s, err := remotefs.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()

	if err := s.Symlink(ctx, "/dir/link", "/library/poem.html"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Stat types it as a symlink with the target length and target.
	fi, err := s.Stat(ctx, "/dir/link")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.Symlink || fi.IsDir {
		t.Errorf("Stat: got Symlink=%v IsDir=%v, want true/false", fi.Symlink, fi.IsDir)
	}
	if fi.LinkTarget != "/library/poem.html" {
		t.Errorf("Stat LinkTarget = %q, want /library/poem.html", fi.LinkTarget)
	}
	if fi.Size != int64(len("/library/poem.html")) {
		t.Errorf("Stat Size = %d, want %d", fi.Size, len("/library/poem.html"))
	}

	// ListDir surfaces the same bit.
	entries, err := s.ListDir(ctx, "/dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || !entries[0].Symlink || entries[0].LinkTarget != "/library/poem.html" {
		t.Errorf("ListDir: got %+v, want one symlink to /library/poem.html", entries)
	}

	// Readlink returns the target; a non-symlink is ErrNotExist.
	target, err := s.Readlink(ctx, "/dir/link")
	if err != nil || target != "/library/poem.html" {
		t.Errorf("Readlink = %q, %v; want /library/poem.html", target, err)
	}
	if err := s.Put(ctx, "/dir/regular.txt", bytes.NewBufferString("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Readlink(ctx, "/dir/regular.txt"); !errors.Is(err, remotefs.ErrNotExist) {
		t.Errorf("Readlink of regular file: got %v, want ErrNotExist", err)
	}

	// Delete removes the link.
	if err := s.Delete(ctx, "/dir/link"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(ctx, "/dir/link"); !errors.Is(err, remotefs.ErrNotExist) {
		t.Errorf("after Delete: got %v, want ErrNotExist", err)
	}
}
