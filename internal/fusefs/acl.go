// Package fusefs is the FUSE filesystem for the sandbox workspace.
// The ACL evaluator in this file is platform-agnostic so it can be unit-tested
// on macOS even though the FUSE mount itself is Linux-only.
package fusefs

import (
	"path"
	"strings"
)

// Access represents an ACL verdict per [DESIGN.md §5].
type Access string

const (
	AccessRW   Access = "rw"
	AccessRO   Access = "ro"
	AccessDeny Access = "deny"
)

// Rule pairs a path pattern with an access level.
// Pattern uses "/**" as a recursive-subtree suffix, matching DESIGN.md §5
// examples. Patterns without "/**" match the exact path only.
type Rule struct {
	Path   string `json:"path"`
	Access Access `json:"access"`
}

// ACLs is the compiled longest-prefix-match policy used by the FUSE server.
// Implements DESIGN.md §8.2: longest-prefix wins, default deny.
type ACLs struct {
	rules []Rule
}

// Compile validates and orders the rules for matching. Order is deterministic
// (longer patterns checked first), so the longest match wins regardless of
// input order.
func Compile(rules []Rule) *ACLs {
	out := make([]Rule, len(rules))
	copy(out, rules)
	// Order: more-specific (longer patterns) first.
	// Length without the "/**" suffix is the discriminator.
	sortByPrefixLengthDesc(out)
	return &ACLs{rules: out}
}

// Eval returns the access verdict for an absolute path. Default = deny.
// p is expected to be an absolute path with forward slashes (the form FUSE
// passes us, normalized via path.Clean).
func (a *ACLs) Eval(p string) Access {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	for _, r := range a.rules {
		if matches(p, r.Path) {
			return r.Access
		}
	}
	return AccessDeny
}

func matches(p, pattern string) bool {
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		// /workspace/** matches /workspace and any descendant.
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	return p == pattern
}

// sortByPrefixLengthDesc orders rules so longer prefixes come first.
// Hand-rolled (no sort import) — N is small.
func sortByPrefixLengthDesc(rs []Rule) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0; j-- {
			if effectiveLen(rs[j].Path) > effectiveLen(rs[j-1].Path) {
				rs[j], rs[j-1] = rs[j-1], rs[j]
			} else {
				break
			}
		}
	}
}

func effectiveLen(p string) int {
	return len(strings.TrimSuffix(p, "/**"))
}
