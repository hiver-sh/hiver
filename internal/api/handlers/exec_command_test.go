package handlers

import (
	"testing"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// TestCommandFromJSON covers the command union normalization: a JSON string is
// passed through verbatim (shell-interpreted), while a JSON array is treated as
// argv and shell-quoted so each element survives `sh -c` as one literal token.
func TestCommandFromJSON(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"string", `"python3 script.py"`, "python3 script.py"},
		{"string with shell ops", `"a && b | c"`, "a && b | c"},
		{"array", `["python3", "script.py"]`, `'python3' 'script.py'`},
		{"array preserves spaces", `["echo", "hello world"]`, `'echo' 'hello world'`},
		{"array quotes metachars", `["echo", "$HOME && rm"]`, `'echo' '$HOME && rm'`},
		{"array escapes single quote", `["echo", "it's"]`, `'echo' 'it'\''s'`},
		{"empty array", `[]`, ""},
		{"empty string", `""`, ""},
		{"null", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := commandFromJSON([]byte(tc.json))
			if err != nil {
				t.Fatalf("commandFromJSON(%s): %v", tc.json, err)
			}
			if got != tc.want {
				t.Errorf("commandFromJSON(%s) = %q, want %q", tc.json, got, tc.want)
			}
		})
	}
}

// TestResolveCommandUnion verifies the generated union wrappers round-trip
// through resolveCommand / resolveCommandOpt to the expected shell command.
func TestResolveCommandUnion(t *testing.T) {
	var argv gen.ExecRequest_Command
	if err := argv.FromExecRequestCommand1([]string{"ls", "-la"}); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCommand(argv)
	if err != nil {
		t.Fatal(err)
	}
	if want := `'ls' '-la'`; got != want {
		t.Errorf("resolveCommand(argv) = %q, want %q", got, want)
	}

	if got, err := resolveCommandOpt(nil); err != nil || got != "" {
		t.Errorf("resolveCommandOpt(nil) = %q, %v; want \"\", nil", got, err)
	}
}
