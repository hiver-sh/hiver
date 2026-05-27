package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBaseDir_GlobStar(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/home/user/*", "/home/user"},
		{"/home/user/**", "/home/user"},
		{"/home/user/", "/home/user"},
		{"/home/user", "/home/user"},
	}
	for _, c := range cases {
		got := baseDir(c.in)
		if got != c.want {
			t.Errorf("baseDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBestMount_NoMounts(t *testing.T) {
	if bestMount(nil, "/foo") != nil {
		t.Fatal("expected nil")
	}
}

func TestBestMount_ExactMatch(t *testing.T) {
	mounts := []MountSource{{ContainerPath: "/workspace", HostDir: "/host/ws"}}
	m := bestMount(mounts, "/workspace")
	if m == nil || m.HostDir != "/host/ws" {
		t.Fatalf("unexpected: %v", m)
	}
}

func TestBestMount_PrefixMatch(t *testing.T) {
	mounts := []MountSource{{ContainerPath: "/workspace", HostDir: "/host/ws"}}
	m := bestMount(mounts, "/workspace/sub/dir")
	if m == nil || m.HostDir != "/host/ws" {
		t.Fatalf("unexpected: %v", m)
	}
}

func TestBestMount_LongestPrefixWins(t *testing.T) {
	mounts := []MountSource{
		{ContainerPath: "/workspace", HostDir: "/host/ws"},
		{ContainerPath: "/workspace/sub", HostDir: "/host/sub"},
	}
	m := bestMount(mounts, "/workspace/sub/file")
	if m == nil || m.HostDir != "/host/sub" {
		t.Fatalf("expected /host/sub, got %v", m)
	}
}

func TestBestMount_NoMatch(t *testing.T) {
	mounts := []MountSource{{ContainerPath: "/workspace", HostDir: "/host/ws"}}
	if bestMount(mounts, "/other") != nil {
		t.Fatal("expected nil")
	}
}

func TestBestMount_PartialNameNotMatch(t *testing.T) {
	// /workspacefoo must NOT match /workspace
	mounts := []MountSource{{ContainerPath: "/workspace", HostDir: "/host/ws"}}
	if bestMount(mounts, "/workspacefoo") != nil {
		t.Fatal("expected nil")
	}
}

// ---- resolveSource ----

func TestResolveSource_UsesUpperDir(t *testing.T) {
	hostRoot, tarPrefix := resolveSource("/upper", nil, "/home/user")
	if hostRoot != "/upper/home/user" {
		t.Errorf("hostRoot = %q", hostRoot)
	}
	if tarPrefix != "home/user" {
		t.Errorf("tarPrefix = %q", tarPrefix)
	}
}

func TestResolveSource_UsesMount(t *testing.T) {
	mounts := []MountSource{{ContainerPath: "/workspace", HostDir: "/host/ws"}}
	hostRoot, tarPrefix := resolveSource("/upper", mounts, "/workspace/proj")
	if hostRoot != "/host/ws/proj" {
		t.Errorf("hostRoot = %q", hostRoot)
	}
	if tarPrefix != "workspace/proj" {
		t.Errorf("tarPrefix = %q", tarPrefix)
	}
}

func TestResolveTarget_UsesUpperDir(t *testing.T) {
	got := resolveTarget("/upper", nil, "home/user/file.txt")
	if got != "/upper/home/user/file.txt" {
		t.Errorf("got %q", got)
	}
}

func TestResolveTarget_UsesMount(t *testing.T) {
	mounts := []MountSource{{ContainerPath: "/workspace", HostDir: "/host/ws"}}
	got := resolveTarget("/upper", mounts, "workspace/proj/main.go")
	if got != "/host/ws/proj/main.go" {
		t.Errorf("got %q", got)
	}
}

func TestResolveTarget_TraversalUpperDir(t *testing.T) {
	got := resolveTarget("/upper", nil, "../escape")
	if got != "" {
		t.Errorf("expected empty for traversal, got %q", got)
	}
}

func TestResolveTarget_TraversalMount(t *testing.T) {
	mounts := []MountSource{{ContainerPath: "/workspace", HostDir: "/host/ws"}}
	got := resolveTarget("/upper", mounts, "workspace/../../etc/passwd")
	if got != "" {
		t.Errorf("expected empty for traversal, got %q", got)
	}
}

// ---- SnapshotPath ----

func TestSnapshotPath(t *testing.T) {
	got := SnapshotPath("/snapshots", "abc123")
	want := "/snapshots/snapshot-abc123.tar.gz"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCaptureRestore_RoundTrip(t *testing.T) {
	// Build a small directory tree to snapshot.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(src, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	// upper dir maps /data → src on the host.
	upper := t.TempDir()
	if err := os.MkdirAll(filepath.Join(upper, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Copy files into upper so resolveSource finds them at /data.
	cp := func(dst, src string) {
		t.Helper()
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cp(filepath.Join(upper, "data", "a.txt"), filepath.Join(src, "a.txt"))
	if err := os.Mkdir(filepath.Join(upper, "data", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cp(filepath.Join(upper, "data", "sub", "b.txt"), filepath.Join(sub, "b.txt"))

	tarPath := filepath.Join(t.TempDir(), "snap.tar.gz")
	if err := Capture(tarPath, upper, nil, []string{"/data"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Restore into a fresh upper dir.
	upper2 := t.TempDir()
	if err := Restore(tarPath, upper2, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	check := func(rel, want string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(upper2, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(b) != want {
			t.Errorf("%s: got %q, want %q", rel, string(b), want)
		}
	}
	check("data/a.txt", "hello")
	check("data/sub/b.txt", "world")
}

func TestCaptureRestore_WithMount(t *testing.T) {
	// Source content lives in a "local FS backend" directory.
	backend := t.TempDir()
	if err := os.WriteFile(filepath.Join(backend, "file.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	mounts := []MountSource{{ContainerPath: "/workspace", HostDir: backend}}
	upper := t.TempDir()

	tarPath := filepath.Join(t.TempDir(), "snap.tar.gz")
	if err := Capture(tarPath, upper, mounts, []string{"/workspace"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	backend2 := t.TempDir()
	mounts2 := []MountSource{{ContainerPath: "/workspace", HostDir: backend2}}
	if err := Restore(tarPath, upper, mounts2); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(backend2, "file.go"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(b) != "package main" {
		t.Errorf("got %q", string(b))
	}
}

func TestCapture_SkipsMissingPath(t *testing.T) {
	upper := t.TempDir()
	tarPath := filepath.Join(t.TempDir(), "snap.tar.gz")
	// /nonexistent does not exist under upper — should succeed with 0 entries.
	if err := Capture(tarPath, upper, nil, []string{"/nonexistent"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// Tar should be valid but empty.
	f, _ := os.Open(tarPath)
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	_, err := tr.Next()
	if err != io.EOF {
		t.Errorf("expected empty tar, got err=%v", err)
	}
}

func TestRestore_BadTar(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := os.WriteFile(bad, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Restore(bad, t.TempDir(), nil); err == nil {
		t.Fatal("expected error for bad gzip")
	}
}
