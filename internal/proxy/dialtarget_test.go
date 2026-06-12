package proxy

import "testing"

func TestDialTarget(t *testing.T) {
	withOverride := func(h string) *EgressRule {
		return &EgressRule{Access: "allow", Host: "api.example.com", Override: &EgressOverride{Host: h}}
	}
	cases := []struct {
		name string
		rule *EgressRule
		def  string
		want string
	}{
		{"nil rule", nil, "api.example.com:443", "api.example.com:443"},
		{"no override", &EgressRule{Access: "allow", Host: "api.example.com"}, "api.example.com:443", "api.example.com:443"},
		{"empty override host", withOverride(""), "api.example.com:443", "api.example.com:443"},
		{"override with port", withOverride("stub.internal:17080"), "api.example.com:443", "stub.internal:17080"},
		{"override without port keeps def port", withOverride("stub.internal"), "api.example.com:443", "stub.internal:443"},
		{"override without port, def without port", withOverride("stub.internal"), "api.example.com", "stub.internal"},
		{"override ip with port", withOverride("192.168.65.2:8080"), "api.example.com:80", "192.168.65.2:8080"},
		{"ipv6 override", withOverride("[::1]:8080"), "api.example.com:443", "[::1]:8080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dialTarget(c.rule, c.def); got != c.want {
				t.Errorf("dialTarget(%+v, %q) = %q, want %q", c.rule, c.def, got, c.want)
			}
		})
	}
}
