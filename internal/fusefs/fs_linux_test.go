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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/remotefs"
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

// countingRemote wraps a Store and tallies the Stat and ListDir round-trips
// the FUSE read path makes, so a test can assert how a probe storm maps onto
// backend calls.
type countingRemote struct {
	remotefs.Store
	mu      sync.Mutex
	stat    int
	listDir int
}

func (c *countingRemote) Stat(ctx context.Context, p string) (remotefs.FileInfo, error) {
	c.mu.Lock()
	c.stat++
	c.mu.Unlock()
	return c.Store.Stat(ctx, p)
}

func (c *countingRemote) ListDir(ctx context.Context, dir string) ([]remotefs.FileInfo, error) {
	c.mu.Lock()
	c.listDir++
	c.mu.Unlock()
	return c.Store.ListDir(ctx, dir)
}

func (c *countingRemote) counts() (statN, listN int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stat, c.listDir
}

func (c *countingRemote) reset() {
	c.mu.Lock()
	c.stat, c.listDir = 0, 0
	c.mu.Unlock()
}

// startFUSERemote mounts a remote-backed workspace: reads consult the remote
// once (served thereafter through the permanent stat cache), and remoteDir is
// the upstream store's backing directory the test seeds objects into.
func startFUSERemote(t *testing.T, remote remotefs.Store) (mountPoint string, stop func()) {
	return startFUSERemoteMode(t, remote, false)
}

// startFUSERemoteAsync mounts the workspace in async (local-authoritative)
// mode: stats never block on the backend.
func startFUSERemoteAsync(t *testing.T, remote remotefs.Store) (mountPoint string, stop func()) {
	return startFUSERemoteMode(t, remote, true)
}

func startFUSERemoteMode(t *testing.T, remote remotefs.Store, async bool) (mountPoint string, stop func()) {
	t.Helper()
	requiresFUSE(t)
	backend := t.TempDir()
	mountPoint = t.TempDir()
	srv, err := fusefs.Mount(fusefs.Config{
		MountPoint: mountPoint,
		Backend:    backend,
		ACLs:       fusefs.Compile([]fusefs.Rule{{Path: path.Clean(mountPoint), Access: fusefs.AccessRW}, {Path: path.Clean(mountPoint + "/**"), Access: fusefs.AccessRW}}),
		Audit:      &bytes.Buffer{},
		Remote:     remote,
		Async:      async,
	})
	if err != nil {
		t.Skipf("fusefs.Mount: %v (FUSE may not be available)", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
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
	return mountPoint, stop
}

// TestFUSEProbeStormCollapsesToOneListDir pins the fix for the negative-lookup
// storm: a tool probing a directory for a batch of config files that don't
// exist (CLAUDE.md, .mcp.json, .rgignore, .git, …) must cost ONE ListDir, not a
// Remote.Stat per name plus a speculative <name>.symlink probe on every miss.
func TestFUSEProbeStormCollapsesToOneListDir(t *testing.T) {
	remoteDir := t.TempDir()
	inner, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	remote := &countingRemote{Store: inner}
	// Seed one real file so the warm listing has content to cache, proving an
	// existing sibling is found from the same ListDir (no extra Stat).
	if err := os.WriteFile(filepath.Join(remoteDir, "real.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mp, stop := startFUSERemote(t, remote)
	defer stop()

	remote.reset()
	probes := []string{"CLAUDE.md", ".mcp.json", ".rgignore", ".git", "HEAD", ".ignore"}
	for _, name := range probes {
		if _, err := os.Stat(filepath.Join(mp, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat(%s) err = %v, want ErrNotExist", name, err)
		}
	}
	statN, listN := remote.counts()
	if listN != 1 {
		t.Errorf("probe storm made %d ListDir calls, want 1", listN)
	}
	if statN != 0 {
		t.Errorf("probe storm made %d per-child Stat calls, want 0 (incl. no .symlink probes)", statN)
	}

	// The real sibling resolves from the same warm listing — no new round-trips.
	remote.reset()
	if _, err := os.Stat(filepath.Join(mp, "real.txt")); err != nil {
		t.Fatalf("Stat(real.txt) = %v, want success", err)
	}
	if statN, listN := remote.counts(); statN != 0 || listN != 0 {
		t.Errorf("existing sibling cost stat=%d list=%d round-trips, want 0/0", statN, listN)
	}

	// Re-probing the misses stays local (dirListed oracle) — still no round-trips.
	remote.reset()
	for _, name := range probes {
		_, _ = os.Stat(filepath.Join(mp, name))
	}
	if statN, listN := remote.counts(); statN != 0 || listN != 0 {
		t.Errorf("re-probe cost stat=%d list=%d round-trips, want 0/0", statN, listN)
	}
}

// TestFUSECreateThenReadBurstNoRelist pins the incremental-cache fix: a local
// create (mkdir/create/symlink) records the new child positively and keeps the
// parent's listing marker, so the common `mkdir -p a b; stat a b` burst re-lists
// the parent ZERO times after the initial warm. Before the fix each create
// dropped the parent marker (invalidate), so every following lookup re-listed.
func TestFUSECreateThenReadBurstNoRelist(t *testing.T) {
	remoteDir := t.TempDir()
	inner, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	remote := &countingRemote{Store: inner}

	mp, stop := startFUSERemote(t, remote)
	defer stop()

	// Warm the parent's listing once (one cold miss lists the mount root).
	remote.reset()
	if _, err := os.Stat(filepath.Join(mp, "probe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("warm Stat err = %v, want ErrNotExist", err)
	}
	if _, listN := remote.counts(); listN != 1 {
		t.Fatalf("warm cost %d ListDir, want 1", listN)
	}

	// The create-then-read burst mirrors the observed `mkdir -p input output;
	// ln -sf … poem.html; stat …` sequence: additive creates followed by reads
	// of the new children and an absent sibling — none of it may touch the
	// backend, because each create records its child and keeps the parent marker.
	remote.reset()
	for _, d := range []string{"input", "output"} {
		if err := os.Mkdir(filepath.Join(mp, d), 0o755); err != nil {
			t.Fatalf("Mkdir(%s): %v", d, err)
		}
	}
	if err := os.Symlink("/library/poem.html", filepath.Join(mp, "poem.html")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for _, name := range []string{"input", "output", "poem.html"} {
		if _, err := os.Lstat(filepath.Join(mp, name)); err != nil {
			t.Errorf("Lstat(%s) after create = %v, want success", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mp, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(absent) err = %v, want ErrNotExist", err)
	}

	statN, listN := remote.counts()
	if listN != 0 {
		t.Errorf("create-then-read burst made %d ListDir calls, want 0 (parent marker preserved)", listN)
	}
	if statN != 0 {
		t.Errorf("create-then-read burst made %d Stat calls, want 0", statN)
	}
}

// TestFUSEReadDirServesStoreOnce pins the read-through listing cache: a repeat
// readdir of the same directory reuses the first ListDir instead of hitting the
// backend again ("read the store once, then serve the local copy"), and a local
// mkdir between reads still appears — via the local-buffer merge — without
// forcing a re-list.
func TestFUSEReadDirServesStoreOnce(t *testing.T) {
	remoteDir := t.TempDir()
	inner, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "seed.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	remote := &countingRemote{Store: inner}

	mp, stop := startFUSERemote(t, remote)
	defer stop()

	readNames := func() map[string]bool {
		t.Helper()
		ents, err := os.ReadDir(mp)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		names := map[string]bool{}
		for _, e := range ents {
			names[e.Name()] = true
		}
		return names
	}

	remote.reset()
	if n := readNames(); !n["seed.txt"] {
		t.Fatalf("first readdir missing seed.txt: %v", n)
	}
	// Second readdir of the same directory is served from the cached listing.
	if n := readNames(); !n["seed.txt"] {
		t.Fatalf("second readdir missing seed.txt: %v", n)
	}
	if _, listN := remote.counts(); listN != 1 {
		t.Errorf("two readdirs made %d ListDir calls, want 1 (store read once)", listN)
	}

	// A local mkdir must appear in the next readdir via the local-buffer merge,
	// with no extra ListDir — the additive create keeps the cached listing.
	if err := os.Mkdir(filepath.Join(mp, "made-locally"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	n := readNames()
	if !n["made-locally"] || !n["seed.txt"] {
		t.Errorf("readdir after local mkdir = %v, want both seed.txt and made-locally", n)
	}
	if _, listN := remote.counts(); listN != 1 {
		t.Errorf("readdir after local mkdir made %d total ListDir calls, want 1", listN)
	}
}

// TestFUSESymlinkServedFromListing pins that a symlink the store reports via
// FileInfo.Symlink is resolved through the FUSE layer with no representation
// knowledge, and — once seen — served from cache without a second round-trip.
func TestFUSESymlinkServedFromListing(t *testing.T) {
	remoteDir := t.TempDir()
	inner, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	remote := &countingRemote{Store: inner}
	// The local FileStore backs symlinks natively, so seed one directly. The
	// FUSE layer only ever sees the FileInfo.Symlink bit the store returns.
	if err := os.Symlink("/library/poem.html", filepath.Join(remoteDir, "link")); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	mp, stop := startFUSERemote(t, remote)
	defer stop()

	remote.reset()
	fi, err := os.Lstat(filepath.Join(mp, "link"))
	if err != nil {
		t.Fatalf("Lstat(link) = %v, want success", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link reported mode %v, want symlink", fi.Mode())
	}
	// Readlink returns the target with no suffix probe on our side.
	if target, err := os.Readlink(filepath.Join(mp, "link")); err != nil || target != "/library/poem.html" {
		t.Fatalf("Readlink(link) = %q, %v; want /library/poem.html", target, err)
	}
	// A second Lstat is served from the symlink cache entry — no new round-trips.
	remote.reset()
	if _, err := os.Lstat(filepath.Join(mp, "link")); err != nil {
		t.Fatalf("second Lstat(link) = %v", err)
	}
	if statN, listN := remote.counts(); statN != 0 || listN != 0 {
		t.Errorf("cached symlink cost stat=%d list=%d round-trips, want 0/0", statN, listN)
	}
}

// blockingRemote parks every Stat on a release channel so a test can prove an
// async mount's Attr does not wait on the backend, then release it and observe
// the background cache populate. Per-path Stat counts let the test assert the
// stat storm collapses to a single in-flight Remote.Stat.
type blockingRemote struct {
	remotefs.Store
	release chan struct{}
	mu      sync.Mutex
	byPath  map[string]int
}

func (b *blockingRemote) Stat(ctx context.Context, p string) (remotefs.FileInfo, error) {
	b.mu.Lock()
	if b.byPath == nil {
		b.byPath = map[string]int{}
	}
	b.byPath[p]++
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-ctx.Done():
		return remotefs.FileInfo{}, ctx.Err()
	}
	return b.Store.Stat(ctx, p)
}

func (b *blockingRemote) count(p string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.byPath[p]
}

// TestFUSEAsyncAttrDoesNotBlockOnRemote pins the async stat behavior: with the
// backend's Stat parked, a cold getattr must return promptly from the local
// view (never blocking on the round-trip), and once the backend is released the
// background refresh populates the cache so a later stat reports the real
// object. A stat storm on the cold path collapses to one Remote.Stat.
func TestFUSEAsyncAttrDoesNotBlockOnRemote(t *testing.T) {
	remoteDir := t.TempDir()
	inner, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "real.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	remote := &blockingRemote{Store: inner, release: make(chan struct{})}

	mp, stop := startFUSERemoteAsync(t, remote)
	defer stop()

	// Backend Stat is parked: a storm of cold stats must still return quickly
	// from the local view (ErrNotExist — the object isn't buffered yet), not
	// block on the round-trip.
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := os.Stat(filepath.Join(mp, "real.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("cold async Stat err = %v, want ErrNotExist", err)
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cold async stat blocked on parked remote: took %v", d)
	}

	// Release the backend; the background refresh populates the cache. After
	// that a stat is a hit and reports the real 2-byte object.
	close(remote.release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		fi, err := os.Stat(filepath.Join(mp, "real.txt"))
		if err == nil {
			if fi.Size() != 2 {
				t.Fatalf("populated stat size = %d, want 2", fi.Size())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async cache never populated: last err = %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The storm plus the populating poll collapsed to a single Remote.Stat for
	// the path — the in-flight dedup held while the first refresh ran.
	if n := remote.count("/real.txt"); n != 1 {
		t.Errorf("Remote.Stat(/real.txt) called %d times, want 1 (dedup)", n)
	}
}
