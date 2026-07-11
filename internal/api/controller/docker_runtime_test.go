package controller

import (
	"reflect"
	"testing"

	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

func TestExtraHostArgs(t *testing.T) {
	hosts := func(h ...string) *[]string { return &h }

	cases := []struct {
		name string
		cfg  sandboxgen.SandboxConfig
		want []string
	}{
		{"nil", sandboxgen.SandboxConfig{}, nil},
		{"empty", sandboxgen.SandboxConfig{ExtraHosts: hosts()}, nil},
		{
			"single host-gateway alias",
			sandboxgen.SandboxConfig{ExtraHosts: hosts("external-fs:host-gateway")},
			[]string{"--add-host", "external-fs:host-gateway"},
		},
		{
			"multiple entries",
			sandboxgen.SandboxConfig{ExtraHosts: hosts("upstream-ws:host-gateway", "api.virtual.test:127.0.0.1")},
			[]string{
				"--add-host", "upstream-ws:host-gateway",
				"--add-host", "api.virtual.test:127.0.0.1",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extraHostArgs(c.cfg)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("extraHostArgs() = %v, want %v", got, c.want)
			}
		})
	}
}
