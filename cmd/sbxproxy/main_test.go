package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

	t.Run("array → all-sources bucket, gen 0, no deny-wait", func(t *testing.T) {
		m, dw, gen, err := readRules(write("arr.json", `[{"access":"allow","host":"example.com"}]`))
		if err != nil {
			t.Fatal(err)
		}
		if gen != 0 {
			t.Errorf("gen = %d, want 0", gen)
		}
		if len(m[""]) != 1 {
			t.Errorf("all-sources bucket = %v, want 1 rule", m[""])
		}
		if dw != nil {
			t.Errorf("deny-wait = %v, want nil", dw)
		}
	})

	t.Run("bare per-source map, gen 0", func(t *testing.T) {
		m, dw, gen, err := readRules(write("map.json", `{"172.16.1.2":[{"access":"allow","host":"a.com"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if gen != 0 {
			t.Errorf("gen = %d, want 0", gen)
		}
		if len(m["172.16.1.2"]) != 1 {
			t.Errorf("source bucket = %v, want 1 rule", m["172.16.1.2"])
		}
		if dw != nil {
			t.Errorf("deny-wait = %v, want nil", dw)
		}
	})

	t.Run("envelope carries generation and per-source deny-wait", func(t *testing.T) {
		m, dw, gen, err := readRules(write("env.json",
			`{"generation":7,"sources":{"172.16.1.2":[{"access":"allow","host":"a.com"}],"172.16.2.2":[]},"deny_wait":{"172.16.1.2":30,"172.16.2.2":0}}`))
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
		// 30s is converted to a Duration; the 0 value is dropped (no wait).
		if dw["172.16.1.2"] != 30*time.Second {
			t.Errorf("deny-wait[172.16.1.2] = %v, want 30s", dw["172.16.1.2"])
		}
		if _, ok := dw["172.16.2.2"]; ok {
			t.Errorf("deny-wait[172.16.2.2] = %v, want absent (0 dropped)", dw["172.16.2.2"])
		}
	})

	t.Run("empty path", func(t *testing.T) {
		m, dw, gen, err := readRules("")
		if err != nil || m != nil || dw != nil || gen != 0 {
			t.Errorf("readRules(\"\") = %v, %v, %d, %v; want nil, nil, 0, nil", m, dw, gen, err)
		}
	})
}
