package fusefs_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/remotefs"
)

// remoteContent reads path off the store, returning ("", false) if it is not
// there (yet). Kept local to this file so the regression runs on any OS (the
// FUSE-backed oplog tests, and their shared helpers, are linux-only).
func remoteContent(t *testing.T, store remotefs.Store, path string) (string, bool) {
	t.Helper()
	rc, err := store.Get(context.Background(), path)
	if err != nil {
		if errors.Is(err, remotefs.ErrNotExist) {
			return "", false
		}
		t.Fatalf("store.Get %s: %v", path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b), true
}

func waitRemoteContent(t *testing.T, store remotefs.Store, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := remoteContent(t, store, path); ok {
			if got != want {
				t.Fatalf("remote %s = %q, want %q", path, got, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("remote %s never received %q", path, want)
}

// TestOplogRenameParkedPut is the regression for the write-on-shutdown snapshot
// (TestSnapshotFuseE2E) breaking once OpPut coalescing landed.
//
// snapshot.Capture — like any safe-save — writes a temp file then renames it
// over the final name. Through the FUSE layer that is: write temp → close
// (enqueue OpPut for the temp, now PARKED on the coalesce window) → rename temp
// to final (which moves the local buffer file to the final name).
//
// The buggy behavior was: the parked Put still pointed at the temp buffer path,
// which the rename had just moved away, so its flush hit a vanished buffer and
// skipped the upload; the paired OpMove then referenced a temp object that was
// never uploaded and failed — the remote received nothing. This drives that
// exact oplog-level sequence and asserts the content lands under the final name.
func TestOplogRenameParkedPut(t *testing.T) {
	remoteDir := t.TempDir()
	bufDir := t.TempDir()

	store, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	oplog := fusefs.NewOplog(store, 64)
	// Coalescing on with a wide window: the temp's Put must still be parked when
	// the rename arrives, reproducing the atomic-write race.
	oplog.SetCoalesce(500*time.Millisecond, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go oplog.Run(ctx)

	const (
		tempVirt = "/snapshot-drive/.snapshot-abcd.tar.gz.tmp"
		dstVirt  = "/snapshot-drive/snapshot-key.tar.gz"
		content  = "hello-from-fuse-snapshot"
	)
	tempBuf := filepath.Join(bufDir, "temp.tmp")
	dstBuf := filepath.Join(bufDir, "final.tar.gz")

	// 1. Temp file written + closed → parked OpPut for the temp name.
	if err := os.WriteFile(tempBuf, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp buffer: %v", err)
	}
	oplog.Enqueue(fusefs.OplogEntry{Type: fusefs.OpPut, Path: tempVirt, BufferPath: tempBuf})
	if !oplog.IsDirty(tempVirt) {
		t.Fatalf("temp Put should be parked (dirty) immediately after enqueue")
	}

	// 2. FUSE Rename moves the local buffer to the final name, then redirects the
	//    parked Put instead of enqueueing a Move.
	if err := os.Rename(tempBuf, dstBuf); err != nil {
		t.Fatalf("rename buffer: %v", err)
	}
	if !oplog.RenameParkedPut(tempVirt, dstVirt, dstBuf) {
		t.Fatalf("RenameParkedPut: parked temp Put not found; the rename would have lost the write")
	}

	// 3. The content must land on the remote under the FINAL name…
	waitRemoteContent(t, store, dstVirt, content)

	// …the temp name must never appear on the remote (no phantom object)…
	if _, ok := remoteContent(t, store, tempVirt); ok {
		t.Errorf("temp name present on remote, want absent")
	}
	// …nothing must have dead-lettered (a broken Move/skip would)…
	if dead := oplog.Dead(); len(dead) != 0 {
		t.Errorf("dead-letter list not empty: %v", dead)
	}
	// …and once the upload lands, the destination is clean (Put balanced by
	//    markClean), so reads fall through to the remote rather than a buffer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && oplog.IsDirty(dstVirt) {
		time.Sleep(10 * time.Millisecond)
	}
	if oplog.IsDirty(dstVirt) {
		t.Errorf("destination still dirty after upload landed; dirty accounting leaked")
	}
}

// TestOplogRenameParkedPutFallback verifies RenameParkedPut reports false once
// the source's Put has already left the parking map (flushed to the queue or the
// remote), so the FUSE Rename falls back to a real OpMove against an object that
// does exist remotely.
func TestOplogRenameParkedPutFallback(t *testing.T) {
	remoteDir := t.TempDir()
	bufDir := t.TempDir()

	store, err := remotefs.NewFileStore(remoteDir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	oplog := fusefs.NewOplog(store, 64)
	oplog.SetCoalesce(0, 0) // disabled: the Put enqueues (and flushes) immediately

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go oplog.Run(ctx)

	const srcVirt = "/workspace/a.txt"
	buf := filepath.Join(bufDir, "a.txt")
	if err := os.WriteFile(buf, []byte("data"), 0o644); err != nil {
		t.Fatalf("write buffer: %v", err)
	}
	oplog.Enqueue(fusefs.OplogEntry{Type: fusefs.OpPut, Path: srcVirt, BufferPath: buf})
	waitRemoteContent(t, store, srcVirt, "data") // fully uploaded, nothing parked

	if oplog.RenameParkedPut(srcVirt, "/workspace/b.txt", filepath.Join(bufDir, "b.txt")) {
		t.Fatalf("RenameParkedPut returned true for an already-flushed Put; caller would skip the needed Move")
	}
}
