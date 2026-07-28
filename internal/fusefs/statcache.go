package fusefs

import (
	"sync"
	"time"

	"github.com/hiver-sh/hiver/internal/remotefs"
)

// statCache memoizes remotefs.FileInfo for a short window so an
// `ls -la` doesn't fan out into N Stat calls after the kernel asks
// for attrs of every entry returned by ReadDirAll. ReadDirAll
// populates the cache from its ListDir result; Attr and Lookup
// consult it before calling Remote.Stat.
//
// It also caches negative results (path not present on the remote) as
// tombstones. Without them, tools that repeatedly probe for files that
// don't exist on a remote-backed mount — `.gitignore`, `.claude`,
// `CLAUDE.md`, ripgrep's `.ignore`, and directory prefixes that have no
// backing object — turn every probe into its own Remote.Stat round-trip.
// A tombstone lets the caller skip that redundant call and fall straight
// through to the local check.
//
// Correctness rules the callers must uphold:
//
//   - Skip the cache when the path is dirty (a pending oplog write
//     means the local buffer, not the remote, is the truth).
//   - Invalidate the entry on every local mutation so a follow-up
//     Stat repopulates from the remote once the upload completes. This
//     clears tombstones too, so a create makes the path visible again.
//
// TTL trades freshness against round-trip count: longer windows
// coalesce more attr storms, but make out-of-band Drive edits
// invisible for that long. 5s default keeps the user-visible
// staleness in the same ballpark as Drive's own propagation delays.
// Tombstones share the same TTL, so an out-of-band remote create is
// visible within the same window as an out-of-band edit.
type statCache struct {
	ttl time.Duration

	mu      sync.RWMutex
	entries map[string]statCacheEntry
}

type statCacheEntry struct {
	info remotefs.FileInfo
	// negative marks a tombstone: the remote had no such path. info is
	// zero and must not be read.
	negative bool
	expires  time.Time
}

func newStatCache(ttl time.Duration) *statCache {
	return &statCache{
		ttl:     ttl,
		entries: make(map[string]statCacheEntry),
	}
}

// lookup returns the unexpired entry for p, evicting it if expired.
// Returns ok=false when the cache is disabled (ttl <= 0), the entry is
// missing, or it has expired.
func (c *statCache) lookup(p string) (statCacheEntry, bool) {
	if c == nil || c.ttl <= 0 {
		return statCacheEntry{}, false
	}
	c.mu.RLock()
	e, ok := c.entries[p]
	c.mu.RUnlock()
	if !ok {
		return statCacheEntry{}, false
	}
	if time.Now().After(e.expires) {
		c.mu.Lock()
		// Re-check under the write lock — another goroutine may have
		// refreshed the entry between our read and write locks.
		if cur, ok := c.entries[p]; ok && time.Now().After(cur.expires) {
			delete(c.entries, p)
		}
		c.mu.Unlock()
		return statCacheEntry{}, false
	}
	return e, true
}

// get returns a cached positive FileInfo when one is present and
// unexpired. A tombstone (negative) entry is not a positive hit, so it
// returns ok=false — callers distinguish "known absent" via knownAbsent.
func (c *statCache) get(p string) (remotefs.FileInfo, bool) {
	e, ok := c.lookup(p)
	if !ok || e.negative {
		return remotefs.FileInfo{}, false
	}
	return e.info, true
}

// knownAbsent reports whether p carries an unexpired tombstone — the
// remote was already found to lack this path. Callers use it to skip a
// redundant Remote.Stat and go straight to the local fallback.
func (c *statCache) knownAbsent(p string) bool {
	e, ok := c.lookup(p)
	return ok && e.negative
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

// putNegative records a tombstone: the remote has no object at p. Safe
// to call when the cache is disabled — it's a no-op.
func (c *statCache) putNegative(p string) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.entries[p] = statCacheEntry{negative: true, expires: time.Now().Add(c.ttl)}
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
