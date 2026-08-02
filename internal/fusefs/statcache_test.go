package fusefs

import (
	"testing"

	"github.com/hiver-sh/hiver/internal/remotefs"
)

func TestStatCacheHitMiss(t *testing.T) {
	c := newStatCache()

	if _, ok := c.get("/x"); ok {
		t.Fatal("empty cache should miss")
	}

	want := remotefs.FileInfo{Path: "/x", Size: 42, IsDir: false}
	c.put("/x", want)
	got, ok := c.get("/x")
	if !ok {
		t.Fatal("expected hit after put")
	}
	if got != want {
		t.Errorf("info mismatch: got %+v, want %+v", got, want)
	}
}

func TestStatCacheInvalidate(t *testing.T) {
	c := newStatCache()
	c.put("/x", remotefs.FileInfo{Path: "/x"})
	c.invalidate("/x")
	if _, ok := c.get("/x"); ok {
		t.Error("expected miss after invalidate")
	}
}

// TestStatCacheNilReceiver pins that a nil cache is a safe no-op so callers
// (pure-local mounts) don't have to nil-check every call.
func TestStatCacheNilReceiver(t *testing.T) {
	var nilCache *statCache
	nilCache.put("/x", remotefs.FileInfo{Path: "/x"})
	nilCache.putNegative("/x")
	nilCache.putSymlink("/x", 1)
	nilCache.putListed("/dir", nil)
	nilCache.invalidate("/x")
	if _, ok := nilCache.get("/x"); ok {
		t.Error("nil cache should miss without panicking")
	}
	if nilCache.knownAbsent("/x") {
		t.Error("nil cache knownAbsent should be false without panicking")
	}
	if _, ok := nilCache.getEntry("/x"); ok {
		t.Error("nil cache getEntry should miss without panicking")
	}
	if nilCache.dirListed("/dir") {
		t.Error("nil cache dirListed should be false without panicking")
	}
}

// TestStatCacheNegative pins the tombstone behaviour: a putNegative makes
// knownAbsent true (skipping the remote round-trip) without registering as a
// positive get hit, and is cleared by invalidate so a freshly-created path
// becomes visible again.
func TestStatCacheNegative(t *testing.T) {
	c := newStatCache()

	if c.knownAbsent("/gone") {
		t.Fatal("empty cache should not report knownAbsent")
	}

	c.putNegative("/gone")
	if !c.knownAbsent("/gone") {
		t.Fatal("expected knownAbsent after putNegative")
	}
	// A tombstone must never masquerade as a positive hit.
	if _, ok := c.get("/gone"); ok {
		t.Error("negative entry must not be a positive get hit")
	}

	// invalidate clears the tombstone (a create must make the path visible).
	c.invalidate("/gone")
	if c.knownAbsent("/gone") {
		t.Error("expected tombstone cleared after invalidate")
	}

	// A positive put over a tombstone wins (and clears the negative flag).
	c.putNegative("/x")
	c.put("/x", remotefs.FileInfo{Path: "/x", Size: 5})
	if c.knownAbsent("/x") {
		t.Error("positive put should overwrite the tombstone")
	}
	if got, ok := c.get("/x"); !ok || got.Size != 5 {
		t.Errorf("expected positive hit after put, got %+v ok=%v", got, ok)
	}
}

// TestStatCacheSymlink pins that a symlink entry is a hit distinct from a
// regular file: get (which serves regular files/dirs) misses it, while
// getEntry surfaces it with symlink=true and the target length, so Attr can
// report it via fillAttrSymlink instead of fillAttrFromRemote.
func TestStatCacheSymlink(t *testing.T) {
	c := newStatCache()
	c.putSymlink("/link", 18)

	if _, ok := c.get("/link"); ok {
		t.Error("get must not surface a symlink as a regular positive hit")
	}
	e, ok := c.getEntry("/link")
	if !ok {
		t.Fatal("getEntry should hit for a cached symlink")
	}
	if !e.symlink {
		t.Error("entry should be flagged symlink")
	}
	if e.negative {
		t.Error("symlink entry must not be negative")
	}
	if e.info.Size != 18 {
		t.Errorf("symlink target length = %d, want 18", e.info.Size)
	}
}

// TestStatCacheDirListed pins the directory-listing oracle: putListed marks a
// directory fully enumerated (storing its listing) so a caller can treat an
// uncached child as known-absent and serve a repeat readdir locally, and a
// mutation under the directory (invalidate of a child) drops it so a freshly
// added/removed child is never answered from a stale listing.
func TestStatCacheDirListed(t *testing.T) {
	c := newStatCache()

	if c.dirListed("/dir") {
		t.Fatal("empty cache should not report a directory as listed")
	}
	infos := []remotefs.FileInfo{{Path: "/dir/a.txt", Size: 3}}
	c.putListed("/dir", infos)
	if !c.dirListed("/dir") {
		t.Fatal("expected dirListed after putListed")
	}
	if got, ok := c.cachedListing("/dir"); !ok || len(got) != 1 {
		t.Errorf("cachedListing = %v ok=%v, want the stored listing", got, ok)
	}

	// invalidating a child drops its parent's listing.
	c.invalidate("/dir/new.txt")
	if c.dirListed("/dir") {
		t.Error("a mutation under the dir must drop its listing")
	}
}

// TestStatCachePermanent pins the read-once, no-refetch contract: entries and
// listings live for the cache's lifetime — there is no TTL — so a listed
// directory and its children stay consistent (an entry can never expire out
// from under a live listing and cause a false ENOENT). Only a local mutation
// drops them.
func TestStatCachePermanent(t *testing.T) {
	c := newStatCache()

	c.put("/dir/a.txt", remotefs.FileInfo{Path: "/dir/a.txt", Size: 3})
	c.putListed("/dir", []remotefs.FileInfo{{Path: "/dir/a.txt", Size: 3}})

	// Both the entry and the listing persist — nothing expires them.
	if _, ok := c.get("/dir/a.txt"); !ok {
		t.Error("entry must persist (no TTL)")
	}
	if infos, ok := c.cachedListing("/dir"); !ok || len(infos) != 1 {
		t.Errorf("listing must persist (no TTL): ok=%v n=%d", ok, len(infos))
	}

	// A local mutation still drops the entry and its parent's listing.
	c.invalidate("/dir/a.txt")
	if _, ok := c.get("/dir/a.txt"); ok {
		t.Error("invalidate must drop the entry")
	}
	if _, ok := c.cachedListing("/dir"); ok {
		t.Error("invalidate must drop the parent's listing")
	}
}
