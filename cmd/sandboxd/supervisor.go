package main

import (
	"context"
	"sync"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
	"github.com/hiver-sh/hiver/internal/spec"
)

// claim is a POST /v1/<key> request handed from the API to main's prewarm park:
// main creates the sandbox under key, restores the snapshot into it, and reports
// the outcome on done so Create can return 201 (or the error).
type claim struct {
	key  string
	done chan error
}

// supervisor is the pod-level owner of the sandbox map (design §5). It satisfies
// handlers.Supervisor so the API server can resolve a sandbox by key, list the
// pod's sandboxes, and create/delete them.
//
// A pod hosts a single sandbox. Without -prewarm it is created from the env
// (HIVE_SPEC) config at boot. With -prewarm the pod snapshots and parks; the
// sandbox is created when a POST /v1/<key> claim arrives (claims is the handoff
// to main), restoring the snapshot into the caller's key. The map + keyed API
// are in place so a later phase can host 0..N of the same image.
type supervisor struct {
	mu        sync.Mutex
	image     string                       // the pod's fixed image; set when the sandbox registers
	sandboxes map[string]*handlers.Sandbox // key → sandbox
	cancels   map[string]context.CancelFunc
	claims    chan *claim // non-nil while a prewarmed pod is unclaimed
	pack      *packState  // non-nil in -pack mode: POST /v1/<key> packs a new sandbox into this pod

	// bootDone closes once main has finished deciding the pod's mode (pack set,
	// boot sandbox registered, or prewarm parked). The API serves before that —
	// GET /v1 answers immediately — so Create waits on this first to avoid racing
	// the boot and wrongly reporting ErrPodOccupied while pack is still being set.
	bootDone chan struct{}
	bootOnce sync.Once

	// lifecycle fans inner-sandbox lifecycle transitions out to GET /v1/events
	// subscribers (the controller holds one connection per pod).
	lifecycle *lifecycleBroker
}

func newSupervisor(prewarm bool) *supervisor {
	s := &supervisor{
		sandboxes: map[string]*handlers.Sandbox{},
		cancels:   map[string]context.CancelFunc{},
		bootDone:  make(chan struct{}),
		lifecycle: newLifecycleBroker(),
	}
	if prewarm {
		s.claims = make(chan *claim)
	}
	return s
}

// bootComplete signals that the pod's mode is set; Create may proceed. Idempotent.
func (s *supervisor) bootComplete() { s.bootOnce.Do(func() { close(s.bootDone) }) }

// register records the sandbox under its key, fixes the pod's image, stores the
// lifecycle cancel that DELETE /v1/<key> invokes, and closes the claim window so
// no further POST can claim this (single-sandbox) pod.
func (s *supervisor) register(sb *handlers.Sandbox, image string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandboxes[sb.Key()] = sb
	s.image = image
	s.cancels[sb.Key()] = cancel
	s.claims = nil
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
// sandbox. On a prewarmed, unclaimed pod it hands the claim to main, which
// restores the snapshot into a sandbox under key and signals completion. A pod
// that is not prewarmed (its sandbox came from env at boot) has no free slot, so
// a request for a different key returns ErrPodOccupied.
func (s *supervisor) Create(ctx context.Context, key string, cfg gen.SandboxConfig) (*handlers.Sandbox, error) {
	// Wait until main has set the pod's mode, so a POST that races the boot sees
	// pack mode rather than falling through to ErrPodOccupied.
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
	ch := s.claims
	s.mu.Unlock()

	// Pack mode: bring a brand-new sandbox up inside this pod (its own netns/IP,
	// overlay, cgroup, and per-source egress), so N keys of the same image share
	// one container (design §6).
	if pack != nil {
		return s.createPacked(ctx, key, cfg)
	}

	if ch == nil {
		return nil, handlers.ErrPodOccupied
	}

	cl := &claim{key: key, done: make(chan error, 1)}
	select {
	case ch <- cl:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case err := <-cl.done:
		if err != nil {
			return nil, err
		}
		sb, _ := s.Sandbox(key)
		return sb, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Delete tears the sandbox for key down by cancelling its lifecycle context.
func (s *supervisor) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	sb, ok := s.sandboxes[key]
	cancel := s.cancels[key]
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
	return nil
}

// configImage returns the image named by a config, or "" when unset.
// specImage returns the image named by the boot spec, or "".
func specImage(sp *spec.Spec) string {
	return sp.Image
}
