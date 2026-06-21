package main

import (
	"os"
	"path/filepath"
	"testing"
)

// readRules accepts three on-disk shapes; the pack-mode envelope must surface
// its generation while the two legacy shapes report generation 0.
func TestReadRulesShapes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("array → all-sources bucket, gen 0", func(t *testing.T) {
		m, gen, err := readRules(write("arr.json", `[{"access":"allow","host":"example.com"}]`))
		if err != nil {
			t.Fatal(err)
		}
		if gen != 0 {
			t.Errorf("gen = %d, want 0", gen)
		}
		if len(m[""]) != 1 {
			t.Errorf("all-sources bucket = %v, want 1 rule", m[""])
		}
	})

	t.Run("bare per-source map, gen 0", func(t *testing.T) {
		m, gen, err := readRules(write("map.json", `{"172.16.1.2":[{"access":"allow","host":"a.com"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if gen != 0 {
			t.Errorf("gen = %d, want 0", gen)
		}
		if len(m["172.16.1.2"]) != 1 {
			t.Errorf("source bucket = %v, want 1 rule", m["172.16.1.2"])
		}
	})

	t.Run("envelope carries generation", func(t *testing.T) {
		m, gen, err := readRules(write("env.json",
			`{"generation":7,"sources":{"172.16.1.2":[{"access":"allow","host":"a.com"}],"172.16.2.2":[]}}`))
		if err != nil {
			t.Fatal(err)
		}
		if gen != 7 {
			t.Errorf("gen = %d, want 7", gen)
		}
		if len(m) != 2 {
			t.Errorf("sources = %d, want 2", len(m))
		}
		if len(m["172.16.1.2"]) != 1 {
			t.Errorf("source bucket = %v, want 1 rule", m["172.16.1.2"])
		}
	})

	t.Run("empty path", func(t *testing.T) {
		m, gen, err := readRules("")
		if err != nil || m != nil || gen != 0 {
			t.Errorf("readRules(\"\") = %v, %d, %v; want nil, 0, nil", m, gen, err)
		}
	})
}
