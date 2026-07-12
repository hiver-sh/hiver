package sandboxd

import (
	"context"
	"log"
	"os"
	"sync"

	"github.com/google/uuid"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
	"github.com/hiver-sh/hiver/internal/podid"
	"github.com/hiver-sh/hiver/internal/spec"
)

// supervisor is the pod-level owner of the sandbox map (design §5). It satisfies
// handlers.Supervisor so the API server can resolve a sandbox by key, list the
// pod's sandboxes, and create/delete them.
//
// The pod is a pack host: it hosts 0..N same-image sandboxes, each created on
// demand by a POST /v1/<key> carrying that sandbox's config (resuming a keyed VM
// snapshot when one exists on disk).
type supervisor struct {
	mu        sync.Mutex
	image     string                       // the pod's fixed image; set when the sandbox registers
	sandboxes map[string]*handlers.Sandbox // key → sandbox
	cancels   map[string]context.CancelFunc
	// snapFlushed[key] closes once that sandbox's teardown has captured its
	// write-on-shutdown snapshot and drained the FUSE oplog (past stopAll), so
	// the tarball is durable on its remote backend. Delete waits on it, making a
	// DELETE (client Shutdown) a durability barrier rather than a fire-and-forget
	// cancel that returns before the async teardown has flushed anything.
	snapFlushed map[string]chan struct{}
	pack        *packState // POST /v1/<key> packs a new sandbox into this pod

	// bootDone closes once main has set up the pack state. The API serves before
	// that — GET /v1 answers immediately — so Create waits on this first to avoid
	// racing the boot and wrongly reporting ErrPodOccupied while pack is still
	// being set.
	bootDone chan struct{}
	bootOnce sync.Once

	// lifecycle fans inner-sandbox lifecycle transitions out to GET /v1/events
	// subscribers (the controller holds one connection per pod).
	lifecycle *lifecycleBroker

	// routingID is this pod's sandbox routing id: its IPv4 (POD_IP) packed into a
	// UUID. CreateSandbox returns it so the create response matches the
	// controller's. uuid.Nil when POD_IP is unset (e.g. the Docker runtime, where
	// the controller assigns the id).
	routingID uuid.UUID
}

func newSupervisor() *supervisor {
	s := &supervisor{
		sandboxes:   map[string]*handlers.Sandbox{},
		cancels:     map[string]context.CancelFunc{},
		snapFlushed: map[string]chan struct{}{},
		bootDone:    make(chan struct{}),
		lifecycle:   newLifecycleBroker(),
	}
	if ip := os.Getenv("POD_IP"); ip != "" {
		if id, err := podid.FromIP(ip); err != nil {
			log.Printf("sandboxd: POD_IP %q: %v; create responses will omit the routing id", ip, err)
		} else {
			s.routingID = id
		}
	}
	return s
}

// RoutingID reports this pod's sandbox routing id. Satisfies handlers.Supervisor.
func (s *supervisor) RoutingID() uuid.UUID { return s.routingID }

// bootComplete signals that the pod's mode is set; Create may proceed. Idempotent.
func (s *supervisor) bootComplete() { s.bootOnce.Do(func() { close(s.bootDone) }) }

// register records the sandbox under its key, fixes the pod's image, and stores
// the lifecycle cancel that DELETE /v1/<key> invokes. (createPacked registers its
// sandboxes inline; this is retained for the supervisor's unit tests.)
func (s *supervisor) register(sb *handlers.Sandbox, image string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandboxes[sb.Key()] = sb
	s.image = image
	s.cancels[sb.Key()] = cancel
}

// Sandbox resolves a sandbox by key.
func (s *supervisor) Sandbox(key string) (*handlers.Sandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.sandboxes[key]
	return sb, ok
}

// List returns the pod's sandboxes.
func (s *supervisor) List() []*handlers.Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*handlers.Sandbox, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		out = append(out, sb)
	}
	return out
}

// SubscribeLifecycle opens a stream of inner-sandbox lifecycle transitions for
// the GET /v1/events handler. Satisfies handlers.Supervisor.
func (s *supervisor) SubscribeLifecycle() (<-chan gen.PodEvent, func()) {
	return s.lifecycle.Subscribe()
}

// Create resolves or creates the sandbox for key. An existing key returns its
// sandbox; a new key brings up a fresh sandbox in this pod (its own netns/IP,
// overlay, cgroup, and per-source egress), so N keys of the same image share one
// container (design §6). The config travels in the request body.
func (s *supervisor) Create(ctx context.Context, key string, cfg gen.SandboxConfig) (*handlers.Sandbox, error) {
	// Wait until main has set up the pack state, so a POST that races the boot
	// finds it ready rather than falling through to ErrPodOccupied.
	select {
	case <-s.bootDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.mu.Lock()
	if sb, ok := s.sandboxes[key]; ok {
		s.mu.Unlock()
		return sb, nil
	}
	pack := s.pack
	s.mu.Unlock()

	if pack != nil {
		return s.createPacked(ctx, key, cfg)
	}

	return nil, handlers.ErrPodOccupied
}

// Delete tears the sandbox for key down by cancelling its lifecycle context and
// waits until the teardown has captured its write-on-shutdown snapshot and
// flushed it to the remote backend (snapFlushed), so a returning DELETE means
// the snapshot is durable — not merely that teardown was kicked off. The wait is
// bounded by ctx (the client's request timeout); the rest of teardown (slot
// release, unmount) continues asynchronously regardless.
func (s *supervisor) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	sb, ok := s.sandboxes[key]
	cancel := s.cancels[key]
	flushed := s.snapFlushed[key]
	s.mu.Unlock()
	if !ok {
		return handlers.ErrSandboxNotFound
	}
	// Flag it stopping so the listing reflects teardown immediately, even though
	// freeing the slot (cancel → teardown goroutine) is asynchronous.
	sb.SetStopping()
	if cancel != nil {
		cancel()
	}
	if flushed != nil {
		select {
		case <-flushed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// configImage returns the image named by a config, or "" when unset.
// specImage returns the image named by the boot spec, or "".
func specImage(sp *spec.Spec) string {
	return sp.Image
}
