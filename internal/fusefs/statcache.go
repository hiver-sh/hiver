package fusefs

import (
	"path"
	"sync"

	"github.com/hiver-sh/hiver/internal/remotefs"
)

// statCache memoizes remotefs.FileInfo for a remote-backed mount so the read
// path reads the store once and then serves the local copy — there is no TTL
// and no refetch. ReadDirAll populates it from its ListDir result and records
// the directory as fully listed; Attr and Lookup consult it before ever
// calling Remote.Stat, and a listed directory answers "child absent" locally.
//
// It also caches negative results (path not present on the remote) as
// tombstones. Without them, tools that repeatedly probe for files that
// don't exist on a remote-backed mount — `.gitignore`, `.claude`,
// `CLAUDE.md`, ripgrep's `.ignore`, and directory prefixes that have no
// backing object — turn every probe into its own Remote.Stat round-trip.
// A tombstone lets the caller skip that redundant call and fall straight
// through to the local check.
//
// Entries live for the mount's lifetime; the only thing that drops one is a
// local mutation. Correctness rules the callers must uphold:
//
//   - Skip the cache when the path is dirty (a pending oplog write
//     means the local buffer, not the remote, is the truth).
//   - Invalidate on every membership mutation (Remove/Rename) and on an
//     in-place attr change (Write/chmod/truncate) so a follow-up read
//     repopulates from the remote. Additive creates instead record the new
//     child directly and keep the parent listing.
//
// Because nothing expires, an out-of-band backend edit (another writer to the
// same store) stays invisible until a local mutation drops the affected entry.
// That is the intended model for a private, per-sandbox mount that nothing
// mutates behind FUSE's back.
type statCache struct {
	mu      sync.RWMutex
	entries map[string]statCacheEntry
	// listed records, per directory, its full ListDir result. Its presence
	// answers two things without a backend round-trip: a child absent from
	// entries is known-absent on the remote (the listing saw every entry —
	// files, sub-dirs, symlinks), and a repeat readdir is served from the
	// stored infos instead of another Remote.ListDir. Dropped only by a
	// membership mutation under the directory. See [statCache.dirListed] and
	// [statCache.cachedListing].
	listed map[string][]remotefs.FileInfo
}

type statCacheEntry struct {
	info remotefs.FileInfo
	// negative marks a tombstone: the remote had no such path. info is
	// zero and must not be read.
	negative bool
	// symlink marks a symlink (reported by the store via FileInfo.Symlink).
	// info.Size is the target length; the other info fields are zero.
	// Distinguished from a regular positive hit so callers report it with
	// fillAttrSymlink, not fillAttrFromRemote.
	symlink bool
}

func newStatCache() *statCache {
	return &statCache{
		entries: make(map[string]statCacheEntry),
		listed:  make(map[string][]remotefs.FileInfo),
	}
}

// lookup returns the cached entry for p. ok is false only on a genuine miss
// (no entry) or a nil cache — nothing expires.
func (c *statCache) lookup(p string) (statCacheEntry, bool) {
	if c == nil {
		return statCacheEntry{}, false
	}
	c.mu.RLock()
	e, ok := c.entries[p]
	c.mu.RUnlock()
	return e, ok
}

// get returns a cached positive FileInfo for a regular file or directory
// when one is present. Tombstones (negative) and symlink entries are not
// regular positive hits, so both return ok=false — callers distinguish them
// via knownAbsent / getEntry.
func (c *statCache) get(p string) (remotefs.FileInfo, bool) {
	e, ok := c.lookup(p)
	if !ok || e.negative || e.symlink {
		return remotefs.FileInfo{}, false
	}
	return e.info, true
}

// getEntry returns the raw cached entry for p (regular, symlink, or
// tombstone) so callers that need to tell them apart — Lookup/Attr — can
// branch on entry kind. ok is false only on a genuine miss; a tombstone is a
// hit with negative=true.
func (c *statCache) getEntry(p string) (statCacheEntry, bool) {
	return c.lookup(p)
}

// knownAbsent reports whether p carries a tombstone — the remote was already
// found to lack this path. Callers use it to skip a redundant Remote.Stat and
// go straight to the local fallback.
func (c *statCache) knownAbsent(p string) bool {
	e, ok := c.lookup(p)
	return ok && e.negative
}

// put records an authoritative Stat result. Nil-safe (no-op on a nil cache).
func (c *statCache) put(p string, info remotefs.FileInfo) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries[p] = statCacheEntry{info: info}
	c.mu.Unlock()
}

// putNegative records a tombstone: the remote has no object at p. Nil-safe.
func (c *statCache) putNegative(p string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries[p] = statCacheEntry{negative: true}
	c.mu.Unlock()
}

// putSymlink records that p is a symlink whose target is size bytes. Nil-safe.
func (c *statCache) putSymlink(p string, size int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries[p] = statCacheEntry{symlink: true, info: remotefs.FileInfo{Path: p, Size: size}}
	c.mu.Unlock()
}

// putListed marks dir as fully enumerated, storing the raw listing (infos) so
// a repeat readdir is served locally and a child absent from it is known-absent
// on the remote. Paired with the per-child entries written during the same
// ListDir. Nil-safe.
func (c *statCache) putListed(dir string, infos []remotefs.FileInfo) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.listed[dir] = infos
	c.mu.Unlock()
}

// dirListed reports whether dir has been fully listed. Callers use it to
// answer "child absent" without a Remote.Stat: if the child existed on the
// remote, the listing that set this marker would have cached it.
func (c *statCache) dirListed(dir string) bool {
	_, ok := c.cachedListing(dir)
	return ok
}

// cachedListing returns dir's stored full listing when it has been listed.
// ReadDirAll uses it to answer a repeat readdir without a backend ListDir; the
// caller still merges local buffer children on top, so the agent's own pending
// creates show even when they post-date this listing.
func (c *statCache) cachedListing(dir string) ([]remotefs.FileInfo, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	infos, ok := c.listed[dir]
	c.mu.RUnlock()
	return infos, ok
}

// invalidate drops the cached entry for a path, and the full-listing record of
// its parent directory, so a follow-up read repopulates from the remote once
// the oplog has flushed. Dropping the parent listing is what keeps the
// dirListed oracle honest when the change is NOT recorded in the cache:
// Remove/Rename change membership, and Write/chmod/truncate leave a file's
// entry stale — in both cases a lingering listing would answer wrongly (a
// removed child, or a stale attr).
//
// Additive mutations that fully record what changed (Create, Mkdir, Symlink)
// do NOT use this — they put the new child positively and keep the parent
// listing, so a create-then-read burst costs no extra ListDir.
func (c *statCache) invalidate(p string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, p)
	delete(c.listed, path.Dir(p))
	c.mu.Unlock()
}
