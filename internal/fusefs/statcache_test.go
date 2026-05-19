package fusefs

import (
	"testing"
	"time"

	"github.com/blasten/hive/internal/remotefs"
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

	// nil receiver must be a no-op so callers don't have to nil-check.
	var nilCache *statCache
	nilCache.put("/x", remotefs.FileInfo{Path: "/x"})
	nilCache.invalidate("/x")
	if _, ok := nilCache.get("/x"); ok {
		t.Error("nil cache should miss without panicking")
	}
}
