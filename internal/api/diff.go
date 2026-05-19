package api

import (
	"encoding/json"
	"reflect"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
)

// FSBase decodes the variant-agnostic fields (mount, backend, acls)
// shared by every FileSystem oneOf member.
func FSBase(fs gen.FileSystem) gen.FileSystemBase {
	var base gen.FileSystemBase
	if b, err := fs.MarshalJSON(); err == nil {
		_ = json.Unmarshal(b, &base)
	}
	return base
}

// diffConfig returns the additions and removals needed to converge
// `current` to `desired`. FileSystems are keyed by mount path; changing
// ACLs or backend on an existing mount shows up as a removal of the old
// entry plus an addition of the new one (callers correlate by mount).
// EgressRules are compared by deep equality — rule identity is the
// rule itself.
func diffConfig(current, desired gen.SandboxConfig) gen.Changes {
	var ch gen.Changes

	addedFS, removedFS := diffFS(current.Fs, desired.Fs)
	if len(addedFS) > 0 || len(removedFS) > 0 {
		ch.Fs = &struct {
			Added   *[]gen.FileSystem `json:"added,omitempty"`
			Removed *[]gen.FileSystem `json:"removed,omitempty"`
		}{
			Added:   nonEmptyFS(addedFS),
			Removed: nonEmptyFS(removedFS),
		}
	}

	addedE, removedE := diffEgress(egressRules(current.Egress), egressRules(desired.Egress))
	if len(addedE) > 0 || len(removedE) > 0 {
		ch.Egress = &struct {
			Added   *[]gen.EgressRule `json:"added,omitempty"`
			Removed *[]gen.EgressRule `json:"removed,omitempty"`
		}{
			Added:   nonEmptyEgress(addedE),
			Removed: nonEmptyEgress(removedE),
		}
	}
	return ch
}

func egressRules(e *gen.Egress) []gen.EgressRule {
	if e == nil || e.Allow == nil {
		return nil
	}
	return *e.Allow
}

func diffFS(current, desired []gen.FileSystem) (added, removed []gen.FileSystem) {
	curByMount := indexFSByMount(current)
	desByMount := indexFSByMount(desired)
	for _, fs := range desired {
		if cur, ok := curByMount[FSBase(fs).Mount]; !ok || !fsEqual(cur, fs) {
			added = append(added, fs)
		}
	}
	for _, fs := range current {
		if des, ok := desByMount[FSBase(fs).Mount]; !ok || !fsEqual(des, fs) {
			removed = append(removed, fs)
		}
	}
	return
}

// fsEqual compares two FileSystem unions by semantic JSON content
// rather than raw bytes — the bytes inside the union differ between
// values that came in via json.Marshal (compact) and json.MarshalIndent
// (re-indented at encode time), which would defeat reflect.DeepEqual.
func fsEqual(a, b gen.FileSystem) bool {
	ba, errA := a.MarshalJSON()
	bb, errB := b.MarshalJSON()
	if errA != nil || errB != nil {
		return false
	}
	var ma, mb any
	if json.Unmarshal(ba, &ma) != nil || json.Unmarshal(bb, &mb) != nil {
		return false
	}
	return reflect.DeepEqual(ma, mb)
}

func indexFSByMount(fs []gen.FileSystem) map[string]gen.FileSystem {
	m := make(map[string]gen.FileSystem, len(fs))
	for _, f := range fs {
		m[FSBase(f).Mount] = f
	}
	return m
}

func diffEgress(current, desired []gen.EgressRule) (added, removed []gen.EgressRule) {
	for _, r := range desired {
		if !containsEgressRule(current, r) {
			added = append(added, r)
		}
	}
	for _, r := range current {
		if !containsEgressRule(desired, r) {
			removed = append(removed, r)
		}
	}
	return
}

func containsEgressRule(rules []gen.EgressRule, r gen.EgressRule) bool {
	for _, x := range rules {
		if reflect.DeepEqual(x, r) {
			return true
		}
	}
	return false
}

func nonEmptyFS(s []gen.FileSystem) *[]gen.FileSystem {
	if len(s) == 0 {
		return nil
	}
	return &s
}

func nonEmptyEgress(s []gen.EgressRule) *[]gen.EgressRule {
	if len(s) == 0 {
		return nil
	}
	return &s
}
