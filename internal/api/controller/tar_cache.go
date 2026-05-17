package controller

import (
	"container/list"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// tarCacheMaxBytes caps the total on-disk size of cached sandbox-image tars.
const tarCacheMaxBytes int64 = 1 << 30 // 1 GiB

// tarCache memoizes `docker save` output keyed by image reference.
//
// Eviction is strict LRU on total byte size.
type tarCache struct {
	mu      sync.Mutex
	dir     string
	maxSize int64
	size    int64
	list    *list.List // front = MRU, back = LRU; values are *tarEntry
	index   map[string]*list.Element
}

type tarEntry struct {
	image string
	path  string
	size  int64
}

func newTarCache(dir string, maxSize int64) *tarCache {
	return &tarCache{
		dir:     dir,
		maxSize: maxSize,
		list:    list.New(),
		index:   make(map[string]*list.Element),
	}
}

// getOrSave returns a path to a docker-archive tar of `image`,
// producing one via `docker save` on a miss and reusing the cached
// file on a hit. The returned path is stable as long as the entry
// stays in the cache; callers that need the bytes must consume them
// (e.g. via `docker cp`) before yielding the cache lock chain.
func (c *tarCache) getOrSave(image string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.index[image]; ok {
		c.list.MoveToFront(el)
		return el.Value.(*tarEntry).path, nil
	}

	// Filename derived from the image ref: slashes/colons are illegal
	// in path components on some filesystems, so fold them to '_'.
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(image)
	path := filepath.Join(c.dir, "hive-agent-"+safe+".tar")
	if out, err := exec.Command("docker", "save", "-o", path, image).CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("docker save %s: %v: %s", image, err, out)
	}
	info, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("stat tar: %w", err)
	}

	entry := &tarEntry{image: image, path: path, size: info.Size()}
	c.index[image] = c.list.PushFront(entry)
	c.size += entry.size

	// Evict from the LRU end until we're back under the cap.
	for c.size > c.maxSize && c.list.Len() > 1 {
		victim := c.list.Back()
		old := victim.Value.(*tarEntry)
		c.list.Remove(victim)
		delete(c.index, old.image)
		c.size -= old.size
		_ = os.Remove(old.path)
	}
	return path, nil
}
