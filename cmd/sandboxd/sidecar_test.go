package main

import (
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// TestFuseOpKind pins the fuse-op → SSE-operation bucketing. The "what
// changed" contract is that deletions surface as their own `delete`
// operation (not folded into `write`), and metadata-only probes map to
// nothing so they stay off the stream.
func TestFuseOpKind(t *testing.T) {
	cases := []struct {
		op   string
		want gen.FSRequestEventOperation
	}{
		{"read", gen.Read},
		{"readdir", gen.Read},
		{"attr", gen.Read},
		{"lookup", gen.Read},
		{"open", gen.Read},
		{"write", gen.Write},
		{"open-write", gen.Write},
		{"create", gen.Write},
		{"mkdir", gen.Write},
		{"truncate", gen.Write},
		{"remove", gen.Delete}, // unlink and the source half of a rename
		{"rename", ""},         // renames are decomposed into write+remove, never emitted directly
		{"bogus", ""},
	}
	for _, tc := range cases {
		if got := fuseOpKind(tc.op); got != tc.want {
			t.Errorf("fuseOpKind(%q) = %q, want %q", tc.op, got, tc.want)
		}
	}
}
