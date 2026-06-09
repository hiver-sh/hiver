package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundCPU(t *testing.T) {
	// Use values that are exactly representable in float32 to avoid rounding surprises.
	cases := []struct {
		in   float32
		want float32
	}{
		{0, 0},
		{1.0, 1.0},
		{1.1, 1.1},
		{1.04, 1.0},  // rounds down to 1.0
		{1.06, 1.1},  // rounds up to 1.1
		{50.0, 50.0},
		{-1.0, -1.0},
	}
	for _, tc := range cases {
		got := roundCPU(tc.in)
		if got != tc.want {
			t.Errorf("roundCPU(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestReadCPUUsageUsec(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "cpu.stat", "usage_usec 123456\nsystem_usec 5000\nuser_usec 9000\n")

	got, err := readCPUUsageUsec(dir)
	if err != nil {
		t.Fatalf("readCPUUsageUsec: %v", err)
	}
	if got != 123456 {
		t.Errorf("got %d, want 123456", got)
	}
}

func TestReadCPUUsageUsec_MissingField(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "cpu.stat", "system_usec 5000\n")

	got, err := readCPUUsageUsec(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestReadCPUUsageUsec_MissingFile(t *testing.T) {
	if _, err := readCPUUsageUsec(t.TempDir()); err == nil {
		t.Error("expected error for missing cpu.stat, got nil")
	}
}

func TestReadMemoryWorkingSet(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "memory.current", "1048576\n") // 1 MiB
	writeTempFile(t, dir, "memory.stat", "inactive_file 262144\nactive_file 100000\n")

	got, err := readMemoryWorkingSet(dir)
	if err != nil {
		t.Fatalf("readMemoryWorkingSet: %v", err)
	}
	if got != 786432 { // 1048576 - 262144
		t.Errorf("got %d, want 786432", got)
	}
}

func TestReadMemoryWorkingSet_InactiveFileExceedsTotal(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "memory.current", "100\n")
	writeTempFile(t, dir, "memory.stat", "inactive_file 200\n")

	got, err := readMemoryWorkingSet(dir)
	if err != nil {
		t.Fatalf("readMemoryWorkingSet: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestReadMemoryWorkingSet_MissingFile(t *testing.T) {
	if _, err := readMemoryWorkingSet(t.TempDir()); err == nil {
		t.Error("expected error for missing memory.current, got nil")
	}
}

func TestReadStatField(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "memory.stat", "active_anon 40960\ninactive_file 131072\nactive_file 65536\n")

	got, err := readStatField(dir, "inactive_file")
	if err != nil {
		t.Fatalf("readStatField: %v", err)
	}
	if got != 131072 {
		t.Errorf("got %d, want 131072", got)
	}
}

func TestReadStatField_MissingField(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "memory.stat", "active_anon 40960\n")

	got, err := readStatField(dir, "inactive_file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestReadStatField_MissingFile(t *testing.T) {
	if _, err := readStatField(t.TempDir(), "inactive_file"); err == nil {
		t.Error("expected error for missing memory.stat, got nil")
	}
}
