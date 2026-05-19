package fusefs

import (
	"sync"
	"time"

	"github.com/blasten/hive/internal/remotefs"
)

// statCache memoizes remotefs.FileInfo for a short window so an
// `ls -la` doesn't fan out into N Stat calls after the kernel asks
// for attrs of every entry returned by ReadDirAll. ReadDirAll
// populates the cache from its ListDir result; Attr and Lookup
// consult it before calling Remote.Stat.
//
// Correctness rules the callers must uphold:
//
//   - Skip the cache when the path is dirty (a pending oplog write
//     means the local buffer, not the remote, is the truth).
//   - Invalidate the entry on every local mutation so a follow-up
//     Stat repopulates from the remote once the upload completes.
//
// TTL trades freshness against round-trip count: longer windows
// coalesce more attr storms, but make out-of-band Drive edits
// invisible for that long. 5s default keeps the user-visible
// staleness in the same ballpark as Drive's own propagation delays.
type statCache struct {
	ttl time.Duration

	mu      sync.RWMutex
	entries map[string]statCacheEntry
}

type statCacheEntry struct {
	info    remotefs.FileInfo
	expires time.Time
}

func newStatCache(ttl time.Duration) *statCache {
	return &statCache{
		ttl:     ttl,
		entries: make(map[string]statCacheEntry),
	}
}

// get returns a cached FileInfo when one is present and unexpired.
// Returns ok=false when the cache is disabled (ttl <= 0), the entry is
// missing, or it has expired (the expired entry is also evicted).
func (c *statCache) get(p string) (remotefs.FileInfo, bool) {
	if c == nil || c.ttl <= 0 {
		return remotefs.FileInfo{}, false
	}
	c.mu.RLock()
	e, ok := c.entries[p]
	c.mu.RUnlock()
	if !ok {
		return remotefs.FileInfo{}, false
	}
	if time.Now().After(e.expires) {
		c.mu.Lock()
		// Re-check under the write lock — another goroutine may have
		// refreshed the entry between our read and write locks.
		if cur, ok := c.entries[p]; ok && time.Now().After(cur.expires) {
			delete(c.entries, p)
		}
		c.mu.Unlock()
		return remotefs.FileInfo{}, false
	}
	return e.info, true
}

// put records an authoritative Stat result. Safe to call when the
// cache is disabled — it's a no-op.
func (c *statCache) put(p string, info remotefs.FileInfo) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.entries[p] = statCacheEntry{info: info, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}

// invalidate drops the cached entry for a path. Called from every
// mutating handler so a follow-up read fetches fresh remote state
// once the oplog has flushed.
func (c *statCache) invalidate(p string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, p)
	c.mu.Unlock()
}
