package fusefs

import (
	"testing"
	"time"

	"github.com/hiver-sh/hiver/internal/remotefs"
)

func TestStatCacheHitMissExpiry(t *testing.T) {
	c := newStatCache(50 * time.Millisecond)

	if _, ok := c.get("/x"); ok {
		t.Fatal("empty cache should miss")
	}

	want := remotefs.FileInfo{Path: "/x", Size: 42, IsDir: false}
	c.put("/x", want)
	got, ok := c.get("/x")
	if !ok {
		t.Fatal("expected hit immediately after put")
	}
	if got != want {
		t.Errorf("info mismatch: got %+v, want %+v", got, want)
	}

	time.Sleep(70 * time.Millisecond)
	if _, ok := c.get("/x"); ok {
		t.Error("expected miss after TTL elapsed")
	}
}

func TestStatCacheInvalidate(t *testing.T) {
	c := newStatCache(time.Hour)
	c.put("/x", remotefs.FileInfo{Path: "/x"})
	c.invalidate("/x")
	if _, ok := c.get("/x"); ok {
		t.Error("expected miss after invalidate")
	}
}

func TestStatCacheDisabled(t *testing.T) {
	c := newStatCache(0)
	c.put("/x", remotefs.FileInfo{Path: "/x"})
	if _, ok := c.get("/x"); ok {
		t.Error("ttl=0 should disable the cache")
	}
	c.putNegative("/x")
	if c.knownAbsent("/x") {
		t.Error("ttl=0 should disable negative caching too")
	}

	// nil receiver must be a no-op so callers don't have to nil-check.
	var nilCache *statCache
	nilCache.put("/x", remotefs.FileInfo{Path: "/x"})
	nilCache.putNegative("/x")
	nilCache.invalidate("/x")
	if _, ok := nilCache.get("/x"); ok {
		t.Error("nil cache should miss without panicking")
	}
	if nilCache.knownAbsent("/x") {
		t.Error("nil cache knownAbsent should be false without panicking")
	}
}

// TestStatCacheNegative pins the tombstone behaviour: a putNegative makes
// knownAbsent true (skipping the remote round-trip) without registering as a
// positive get hit, expires on the TTL, and is cleared by invalidate so a
// freshly-created path becomes visible again.
func TestStatCacheNegative(t *testing.T) {
	c := newStatCache(50 * time.Millisecond)

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

	// Tombstones expire on the TTL like positive entries.
	c.putNegative("/gone")
	time.Sleep(70 * time.Millisecond)
	if c.knownAbsent("/gone") {
		t.Error("expected tombstone to expire after TTL")
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
