package api

import (
	"sync"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/api/handlers"
)

// ConfigStore holds the active SandboxConfig in memory. It is seeded with the
// boot config and updated through Apply (PUT /v1/config); there is no on-disk
// copy — sandboxd is configured by the HIVE_SPEC env var, and runtime changes
// live only for the life of the process.
type ConfigStore struct {
	// applyMu gates Apply so only one is in flight at a time (TryLock →
	// ErrApplyInProgress); mu guards the cfg field for concurrent Get/Apply.
	applyMu sync.Mutex
	mu      sync.RWMutex
	cfg     gen.SandboxConfig
}

// NewConfigStore seeds the store with the initial (boot) config.
func NewConfigStore(initial gen.SandboxConfig) *ConfigStore {
	return &ConfigStore{cfg: initial}
}

// Get returns the current configuration.
func (s *ConfigStore) Get() (gen.SandboxConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, nil
}

// Apply replaces the configuration with desired and returns the changes (the
// diff against the previous state). Returns ErrApplyInProgress when another
// Apply is already in flight.
func (s *ConfigStore) Apply(desired gen.SandboxConfig) (gen.Changes, error) {
	if !s.applyMu.TryLock() {
		return gen.Changes{}, handlers.ErrApplyInProgress
	}
	defer s.applyMu.Unlock()

	s.mu.RLock()
	current := s.cfg
	s.mu.RUnlock()

	changes := diffConfig(current, desired)

	s.mu.Lock()
	s.cfg = desired
	s.mu.Unlock()
	return changes, nil
}
