package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/api/handlers"
)

// ConfigStore persists the active SandboxConfig as a JSON document on
// disk.
type ConfigStore struct {
	path string
	mu   sync.Mutex
}

func NewConfigStore(path string) *ConfigStore {
	return &ConfigStore{path: path}
}

// Path returns the on-disk location backing the store.
func (s *ConfigStore) Path() string { return s.path }

// Get returns the configuration currently on disk.
func (s *ConfigStore) Get() (gen.SandboxConfig, error) {
	return readConfig(s.path)
}

// Apply replaces the on-disk configuration with desired and returns the
// changes (the diff against the previous on-disk state). Returns
// ErrApplyInProgress when another Apply is already in flight; on a
// write failure the on-disk state is left untouched.
func (s *ConfigStore) Apply(desired gen.SandboxConfig) (gen.Changes, error) {
	if !s.mu.TryLock() {
		return gen.Changes{}, handlers.ErrApplyInProgress
	}
	defer s.mu.Unlock()

	current, err := readConfig(s.path)
	if err != nil {
		return gen.Changes{}, fmt.Errorf("read current config: %w", err)
	}
	changes := diffConfig(current, desired)
	if err := writeConfigAtomic(s.path, desired); err != nil {
		return gen.Changes{}, fmt.Errorf("write new config: %w", err)
	}
	return changes, nil
}

func readConfig(path string) (gen.SandboxConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return gen.SandboxConfig{}, err
	}
	var cfg gen.SandboxConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return gen.SandboxConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// writeConfigAtomic writes cfg to path via temp-file + rename so a
// crash mid-write can't leave a half-written file on disk.
func writeConfigAtomic(path string, cfg gen.SandboxConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
