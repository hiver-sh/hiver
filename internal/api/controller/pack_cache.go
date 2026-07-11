package controller

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"
)

// packCachePollInterval is how often the background poller refreshes the
// prewarm-host cache from the orchestrator. It is the controller's only periodic
// pod List for pack hosts: getOrCreate and the events stream both read the
// resulting in-memory snapshot rather than hitting the API on the hot path.
const packCachePollInterval = 1 * time.Second

// packCache is an in-memory snapshot of the running prewarm/pack host pods,
// mapping each served image to the IPs of the hosts serving it. A single
// background poller (startPackCachePoller) keeps it current; getOrCreate reads it
// to place a sandbox into one of those hosts without an orchestrator round-trip,
// and the events stream reads it to discover which hosts to follow. Reads take a read
// lock and copy, so a refresh never blocks them for long.
type packCache struct {
	mu      sync.RWMutex
	byImage map[string][]string // image → sorted host pod IPs
}

// set replaces the cache contents with a freshly-polled snapshot.
func (c *packCache) set(byImage map[string][]string) {
	c.mu.Lock()
	c.byImage = byImage
	c.mu.Unlock()
}

// ips returns the IPs of every cached host across all images, for the events
// stream to open one connection per host. A pod serves a single image, so the
// per-image slices never overlap.
func (c *packCache) ips() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for _, ips := range c.byImage {
		out = append(out, ips...)
	}
	return out
}

// startPackCachePoller launches the single background loop that keeps the
// prewarm-host cache current. It is the controller's only periodic pod List for
// pack hosts; getOrCreate and the events stream read the cache instead of listing
// pods themselves, keeping the hot path off the k8s API.
func (r *K8sRuntime) startPackCachePoller(ctx context.Context) {
	go func() {
		t := time.NewTicker(packCachePollInterval)
		defer t.Stop()
		r.refreshPackCache(ctx) // populate before serving rather than after one tick
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.refreshPackCache(ctx)
			}
		}
	}()
}

// refreshPackCache rebuilds the image→host-IP map from a single pod List. Hosts
// without an IP yet (still scheduling) are skipped — they aren't reachable for a
// POST anyway. IPs are sorted so candidates' selection is stable across refreshes.
func (r *K8sRuntime) refreshPackCache(ctx context.Context) {
	packs, err := r.listPackPods(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("controller: pack cache refresh: %v", err)
		}
		return
	}
	byImage := map[string][]string{}
	for _, pod := range packs {
		ip := pod.Status.PodIP
		if ip == "" {
			continue
		}
		img, ok := prewarmPodImage(pod)
		if !ok {
			continue
		}
		byImage[img] = append(byImage[img], ip)
	}
	for img := range byImage {
		sort.Strings(byImage[img])
	}
	r.packs.set(byImage)
}
