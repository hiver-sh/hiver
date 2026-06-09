package fusefs_test

import (
	"testing"

	"github.com/hiver-sh/hiver/internal/fusefs"
)

func TestACLLongestPrefixWins(t *testing.T) {
	acl := fusefs.Compile([]fusefs.Rule{
		{Path: "/workspace/**", Access: fusefs.AccessRW},
		{Path: "/workspace/secret/**", Access: fusefs.AccessDeny},
		{Path: "/workspace/readme.md", Access: fusefs.AccessRO},
	})

	cases := []struct {
		path string
		want fusefs.Access
	}{
		{"/workspace", fusefs.AccessRW},
		{"/workspace/main.go", fusefs.AccessRW},
		{"/workspace/secret", fusefs.AccessDeny},
		{"/workspace/secret/keys.txt", fusefs.AccessDeny},
		{"/workspace/readme.md", fusefs.AccessRO},
		{"/workspace/readme.md/x", fusefs.AccessRW}, // exact-match doesn't extend; falls through to /workspace/**
		{"/etc/passwd", fusefs.AccessDeny},          // no rule → default deny
		{"/", fusefs.AccessDeny},
	}
	for _, tc := range cases {
		if got := acl.Eval(tc.path); got != tc.want {
			t.Errorf("Eval(%q) = %s, want %s", tc.path, got, tc.want)
		}
	}
}

func TestACLDefaultDeny(t *testing.T) {
	acl := fusefs.Compile(nil)
	if got := acl.Eval("/anything"); got != fusefs.AccessDeny {
		t.Errorf("default verdict = %s, want deny", got)
	}
}
