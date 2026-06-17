package main

import (
	"sort"
	"testing"

	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/spec"
)

func fs(mount string) spec.FS {
	return spec.FS{Mount: mount, Backend: spec.BackendLocal}
}

func mounts(fsList []spec.FS) []string {
	out := make([]string, len(fsList))
	for i, f := range fsList {
		out[i] = f.Mount
	}
	sort.Strings(out)
	return out
}

func TestPlanReconcile(t *testing.T) {
	tests := []struct {
		name       string
		live       []string
		desired    []spec.FS
		wantAdd    []string
		wantRemove []string
		wantKeep   []string
	}{
		{
			name:    "fresh boot: everything is added",
			live:    nil,
			desired: []spec.FS{fs("/workspace"), fs("/data")},
			wantAdd: []string{"/data", "/workspace"},
		},
		{
			name:     "no change: everything is kept",
			live:     []string{"/workspace"},
			desired:  []spec.FS{fs("/workspace")},
			wantKeep: []string{"/workspace"},
		},
		{
			name:       "add one, remove one, keep one",
			live:       []string{"/workspace", "/old"},
			desired:    []spec.FS{fs("/workspace"), fs("/new")},
			wantAdd:    []string{"/new"},
			wantRemove: []string{"/old"},
			wantKeep:   []string{"/workspace"},
		},
		{
			name:       "empty desired removes all live",
			live:       []string{"/workspace", "/data"},
			desired:    nil,
			wantRemove: []string{"/data", "/workspace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			liveSet := map[string]bool{}
			for _, mt := range tt.live {
				liveSet[mt] = true
			}
			add, remove, keep := planFsReconcile(liveSet, tt.desired)

			sort.Strings(remove)
			if got := mounts(add); !equal(got, tt.wantAdd) {
				t.Errorf("add = %v, want %v", got, tt.wantAdd)
			}
			if !equal(remove, tt.wantRemove) {
				t.Errorf("remove = %v, want %v", remove, tt.wantRemove)
			}
			if got := mounts(keep); !equal(got, tt.wantKeep) {
				t.Errorf("keep = %v, want %v", got, tt.wantKeep)
			}
		})
	}
}

// TestDefaultedACLs verifies an FS with no ACLs gets the open default so a
// reconcile-added mount (fed from on-disk config that never ran spec.Validate)
// is usable rather than default-deny.
func TestDefaultedACLs(t *testing.T) {
	got := defaultedACLs(fs("/workspace"))
	want := []fusefs.Rule{{Path: "/workspace/**", Access: fusefs.AccessRW}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("defaultedACLs = %v, want %v", got, want)
	}

	explicit := spec.FS{Mount: "/x", Backend: spec.BackendLocal, ACLs: []fusefs.Rule{{Path: "/x/sub", Access: fusefs.AccessRO}}}
	if got := defaultedACLs(explicit); len(got) != 1 || got[0].Access != fusefs.AccessRO {
		t.Fatalf("defaultedACLs overrode explicit ACLs: %v", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
