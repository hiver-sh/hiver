package proxy

import "testing"

// TestRulesForSource verifies per-source egress isolation: each source IP is
// governed by its own bucket, the "" bucket is the all-sources fallback, and an
// unknown source on a per-source map is default-deny.
func TestRulesForSource(t *testing.T) {
	p := &Proxy{}
	p.SetRulesBySource(map[string][]EgressRule{
		"10.0.0.2": {{Access: "allow", Host: "a.com"}},
		"10.0.0.3": {{Access: "allow", Host: "b.com"}},
	})

	allowed := func(srcIP, host string) bool {
		r := MatchEgress(p.rulesForSource(srcIP), "GET", host, 443, "/")
		return r != nil && r.Access == "allow"
	}

	// Each source sees only its own allowlist.
	if !allowed("10.0.0.2", "a.com") {
		t.Errorf("10.0.0.2 should reach a.com")
	}
	if allowed("10.0.0.2", "b.com") {
		t.Errorf("10.0.0.2 must NOT reach b.com (that's 10.0.0.3's rule)")
	}
	if !allowed("10.0.0.3", "b.com") {
		t.Errorf("10.0.0.3 should reach b.com")
	}
	if allowed("10.0.0.3", "a.com") {
		t.Errorf("10.0.0.3 must NOT reach a.com")
	}

	// Unknown source with no "" bucket → deny-all (nil rules).
	if p.rulesForSource("10.0.0.9") != nil {
		t.Errorf("unknown source must get no rules (deny-all)")
	}

	// SetRules installs an all-sources "" bucket that applies to every source.
	p.SetRules([]EgressRule{{Access: "allow", Host: "c.com"}})
	if !allowed("1.2.3.4", "c.com") {
		t.Errorf("\"\" bucket should apply to any source")
	}
	if allowed("1.2.3.4", "a.com") {
		t.Errorf("old per-source rules must be gone after SetRules")
	}
}

func TestSrcIPOf(t *testing.T) {
	cases := map[string]string{
		"10.0.0.2:54321": "10.0.0.2",
		"[fe80::1]:443":  "fe80::1",
		"barehost":       "barehost",
	}
	for in, want := range cases {
		if got := srcIPOf(in); got != want {
			t.Errorf("srcIPOf(%q) = %q, want %q", in, got, want)
		}
	}
}
