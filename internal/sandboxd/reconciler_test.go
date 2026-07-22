package sandboxd

import (
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

func TestSpecFromConfigMitmDefaultLeavesRulesAlone(t *testing.T) {
	cfg := gen.SandboxConfig{
		Fs:     []gen.FileSystem{},
		Egress: &[]gen.EgressRule{{Access: "allow", Host: "example.com"}},
	}
	sp, err := specFromConfig(cfg)
	if err != nil {
		t.Fatalf("specFromConfig: %v", err)
	}
	if len(sp.Egress) != 1 || sp.Egress[0].Passthrough {
		t.Fatalf("got egress %+v, want one rule with Passthrough=false (mitm unset defaults to on)", sp.Egress)
	}
}

func TestSpecFromConfigMitmFalseForcesPassthrough(t *testing.T) {
	mitm := false
	cfg := gen.SandboxConfig{
		Fs:     []gen.FileSystem{},
		Mitm:   &mitm,
		Egress: &[]gen.EgressRule{{Access: "allow", Host: "example.com"}, {Access: "deny", Host: "other.com"}},
	}
	sp, err := specFromConfig(cfg)
	if err != nil {
		t.Fatalf("specFromConfig: %v", err)
	}
	if len(sp.Egress) != 2 {
		t.Fatalf("got %d egress rules, want 2", len(sp.Egress))
	}
	for i, rule := range sp.Egress {
		if !rule.Passthrough {
			t.Errorf("egress[%d].Passthrough = false, want true (mitm=false)", i)
		}
	}
}

func TestSpecFromConfigMitmTrueLeavesRulesAlone(t *testing.T) {
	mitm := true
	cfg := gen.SandboxConfig{
		Fs:     []gen.FileSystem{},
		Mitm:   &mitm,
		Egress: &[]gen.EgressRule{{Access: "allow", Host: "example.com"}},
	}
	sp, err := specFromConfig(cfg)
	if err != nil {
		t.Fatalf("specFromConfig: %v", err)
	}
	if len(sp.Egress) != 1 || sp.Egress[0].Passthrough {
		t.Fatalf("got egress %+v, want Passthrough=false (mitm=true)", sp.Egress)
	}
}
