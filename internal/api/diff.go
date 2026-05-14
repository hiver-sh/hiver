package api

import (
	"reflect"

	"github.com/sandbox-platform/agent-sandbox/internal/api/gen"
)

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
		if cur, ok := curByMount[fs.Mount]; !ok || !reflect.DeepEqual(cur, fs) {
			added = append(added, fs)
		}
	}
	for _, fs := range current {
		if des, ok := desByMount[fs.Mount]; !ok || !reflect.DeepEqual(des, fs) {
			removed = append(removed, fs)
		}
	}
	return
}

func indexFSByMount(fs []gen.FileSystem) map[string]gen.FileSystem {
	m := make(map[string]gen.FileSystem, len(fs))
	for _, f := range fs {
		m[f.Mount] = f
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
