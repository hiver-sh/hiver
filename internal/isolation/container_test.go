package isolation

import (
	"slices"
	"testing"
)

func TestContainerExecArgs(t *testing.T) {
	const containerID = "agent-1"
	c := &container{containerID: containerID}
	cwd := "/workspace"
	tests := []struct {
		name    string
		cfg     ExecConfig
		pidFile string
		want    []string
	}{
		{
			name: "minimal",
			cfg:  ExecConfig{Command: "echo hi"},
			want: []string{"exec", containerID, "sh", "-c", "echo hi"},
		},
		{
			name: "cwd and tty",
			cfg:  ExecConfig{Command: "echo hi", Cwd: &cwd, TTY: true},
			want: []string{"exec", "--tty", "--cwd", "/workspace", containerID, "sh", "-c", "echo hi"},
		},
		{
			name: "env sorted deterministically",
			cfg:  ExecConfig{Command: "env", Env: &map[string]string{"FOO": "1", "BAR": "2"}},
			want: []string{"exec", "--env", "BAR=2", "--env", "FOO=1", containerID, "sh", "-c", "env"},
		},
		{
			name: "nil env adds no flags",
			cfg:  ExecConfig{Command: "env"},
			want: []string{"exec", containerID, "sh", "-c", "env"},
		},
		{
			name:    "pid file",
			cfg:     ExecConfig{Command: "echo hi"},
			pidFile: "/tmp/x.pid",
			want:    []string{"exec", "--pid-file", "/tmp/x.pid", containerID, "sh", "-c", "echo hi"},
		},
		{
			name:    "all flags ordered",
			cfg:     ExecConfig{Command: "run", Cwd: &cwd, TTY: true, Env: &map[string]string{"A": "1"}},
			pidFile: "/tmp/x.pid",
			want:    []string{"exec", "--tty", "--cwd", "/workspace", "--pid-file", "/tmp/x.pid", "--env", "A=1", containerID, "sh", "-c", "run"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.execArgs(tt.cfg, tt.pidFile)
			if !slices.Equal(got, tt.want) {
				t.Errorf("execArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParsePPIDStat(t *testing.T) {
	tests := []struct {
		name   string
		stat   string
		want   int
		wantOK bool
	}{
		{
			name:   "simple comm",
			stat:   "1234 (bash) S 1000 1234 1234 0 -1 ...",
			want:   1000,
			wantOK: true,
		},
		{
			name:   "comm with spaces and parens",
			stat:   "4242 (weird )name (x) R 77 4242 ...",
			want:   77,
			wantOK: true,
		},
		{
			name:   "comm with trailing paren content",
			stat:   "5 (a) S 2",
			want:   2,
			wantOK: true,
		},
		{
			name:   "no closing paren",
			stat:   "5 (a S 2",
			wantOK: false,
		},
		{
			name:   "truncated after comm",
			stat:   "5 (a)",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePPIDStat(tt.stat)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("ppid = %d, want %d", got, tt.want)
			}
		})
	}
}
