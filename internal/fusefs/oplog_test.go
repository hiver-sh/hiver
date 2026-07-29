//go:build linux

package fusefs_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/remotefs"
)

// countingStore wraps a [remotefs.Store] and counts Put calls per path so a
// test can assert how many whole-object uploads a burst of writes produced.
type countingStore struct {
	remotefs.Store
	mu   sync.Mutex
	puts map[string]int
}

func newCountingStore(inner remotefs.Store) *countingStore {
	return &countingStore{Store: inner, puts: map[string]int{}}
}

func (c *countingStore) Put(ctx context.Context, path string, content io.Reader) error {
	c.mu.Lock()
	c.puts[path]++
	c.mu.Unlock()
	return c.Store.Put(ctx, path, content)
}

func (c *countingStore) putCount(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts[path]
}

// TestOplogCoalescesPutBurst drives the Oplog directly: a burst of OpPuts for
// the same path enqueued inside the coalesce window must collapse to a single
// store Put (the file an agent builds by repeated append-then-close), yet the
// remote must end up holding the newest bytes.
func TestOplogCoalescesPutBurst(t *testing.T) {
	remoteDir := t.TempDir()
	bufDir := t.TempDir()

	inner, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	store := newCountingStore(inner)

	oplog := fusefs.NewOplog(store, 64)
	// Wide window, small max so the burst below (10 puts ~5ms apart) coalesces
	// but the test doesn't wait seconds.
	oplog.SetCoalesce(200*time.Millisecond, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go oplog.Run(ctx)

	const path = "/workspace/output/poem.html"
	bufPath := filepath.Join(bufDir, "poem.html")

	// 10 appends: each rewrites the whole buffer (as the FUSE layer's buffer
	// does) then enqueues an OpPut, all inside one window.
	var last string
	for i := 0; i < 10; i++ {
		last += "line\n"
		if err := os.WriteFile(bufPath, []byte(last), 0o644); err != nil {
			t.Fatalf("write buffer: %v", err)
		}
		oplog.Enqueue(fusefs.OplogEntry{Type: fusefs.OpPut, Path: path, BufferPath: bufPath})
		time.Sleep(5 * time.Millisecond)
	}

	// After the window elapses the coalesced Put lands with the final content.
	waitForRemote(t, store, path, last)

	if got := store.putCount(path); got != 1 {
		t.Fatalf("coalesced burst produced %d store Puts, want 1", got)
	}
}

// TestOplogCoalesceDisabled verifies the escape hatch: with the window off,
// every OpPut enqueues immediately, so N appends produce N store Puts (the
// pre-coalesce behavior).
func TestOplogCoalesceDisabled(t *testing.T) {
	remoteDir := t.TempDir()
	bufDir := t.TempDir()

	inner, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	store := newCountingStore(inner)

	oplog := fusefs.NewOplog(store, 64)
	oplog.SetCoalesce(0, 0) // disabled

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const path = "/workspace/notes.txt"

	// Each append gets its own buffer file — the real FUSE layer re-creates
	// the buffer before every append, so a Put's post-flush eviction never
	// starves the next. With coalescing off, the three enqueues are three Puts.
	var last string
	for i := 0; i < 3; i++ {
		last += "x"
		bufPath := filepath.Join(bufDir, "notes."+string(rune('a'+i)))
		if err := os.WriteFile(bufPath, []byte(last), 0o644); err != nil {
			t.Fatalf("write buffer: %v", err)
		}
		oplog.Enqueue(fusefs.OplogEntry{Type: fusefs.OpPut, Path: path, BufferPath: bufPath})
	}
	go oplog.Run(ctx)

	waitForRemote(t, store, path, last)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && store.putCount(path) < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := store.putCount(path); got != 3 {
		t.Fatalf("coalescing-disabled produced %d store Puts, want 3", got)
	}
}

// TestOplogReplaysFsMutations writes / renames / removes through a
// FUSE mount whose Config carries an Oplog targeting a [remotefs.Store].
// We assert the mutations replicate to the store with the same paths
// the agent used.
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

// TestOplogAsyncKeepsBuffer regression-tests the async-mount write path:
// the oplog must NOT evict the local buffer copy after a successful Put,
// because async read handlers serve the buffer exclusively — with
// eviction, a written file would vanish from the mount the moment the
// uploader flushed it.
func TestOplogAsyncKeepsBuffer(t *testing.T) {
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
		Remote:     store,
		Async:      true,
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

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mountPoint); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	src := filepath.Join(mountPoint, "kept.txt")
	if err := os.WriteFile(src, []byte("still here"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Wait until the Put lands on the store — the point where the old code
	// evicted the buffer.
	waitForRemote(t, store, filepath.Clean(mountPoint+"/kept.txt"), "still here")

	// The buffer copy must survive the flush…
	if _, err := os.Stat(filepath.Join(backend, "kept.txt")); err != nil {
		t.Fatalf("buffer copy evicted after Put: %v", err)
	}
	// …and the file must still be readable through the mount.
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read through mount after flush: %v", err)
	}
	if string(got) != "still here" {
		t.Fatalf("read through mount after flush: got %q, want %q", got, "still here")
	}
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
