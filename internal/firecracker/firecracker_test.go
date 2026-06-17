package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigJSONShape(t *testing.T) {
	cfg := Config{
		BootSource:    BootSource{KernelImagePath: "/k/vmlinux", BootArgs: "console=ttyS0"},
		MachineConfig: MachineConfig{VcpuCount: 2, MemSizeMib: 512},
		Drives: []Drive{
			{DriveID: RootDriveID, PathOnHost: "/img/rootfs.ext4", IsRootDevice: true, IsReadOnly: true},
		},
		Vsock: &Vsock{GuestCID: 3, UDSPath: "/run/vsock.sock"},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"boot-source", "machine-config", "drives", "vsock"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("config missing key %q; got keys %v", key, keys(raw))
		}
	}
	// network-interfaces is omitempty and absent here.
	if _, ok := raw["network-interfaces"]; ok {
		t.Errorf("network-interfaces should be omitted when empty")
	}
}

func TestDriveJSONKeys(t *testing.T) {
	b, _ := json.Marshal(Drive{DriveID: "rootfs", PathOnHost: "/x", IsRootDevice: true, IsReadOnly: false})
	got := string(b)
	for _, want := range []string{`"drive_id":"rootfs"`, `"path_on_host":"/x"`, `"is_root_device":true`, `"is_read_only":false`} {
		if !strings.Contains(got, want) {
			t.Errorf("drive JSON %s missing %s", got, want)
		}
	}
}

func TestCommand(t *testing.T) {
	bin, args := Command("", "/run/api.sock", "/run/config.json")
	if bin != "firecracker" {
		t.Errorf("bin = %q, want firecracker", bin)
	}
	want := []string{"--api-sock", "/run/api.sock", "--config-file", "/run/config.json"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestCommandNoConfig(t *testing.T) {
	bin, args := CommandNoConfig("", "/run/api.sock")
	if bin != "firecracker" {
		t.Errorf("bin = %q, want firecracker", bin)
	}
	want := []string{"--api-sock", "/run/api.sock"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", args, want)
	}
	// A snapshot resume must not pass a --config-file (the snapshot supplies the
	// machine state); the boot-config flag belongs only to Command.
	for _, a := range args {
		if a == "--config-file" {
			t.Errorf("CommandNoConfig must not include --config-file: %v", args)
		}
	}
}

// TestSnapshotRequests verifies CreateSnapshot/LoadSnapshot hit the right paths
// with the JSON shape Firecracker's snapshot API expects.
func TestSnapshotRequests(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type capture struct {
		method string
		path   string
		body   map[string]json.RawMessage
	}
	got := make(chan capture, 2)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- capture{method: r.Method, path: r.URL.Path, body: body}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	c := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.CreateSnapshot(ctx, "/snap/state", "/snap/mem"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	create := <-got
	if create.method != http.MethodPut || create.path != "/snapshot/create" {
		t.Errorf("create = %s %s, want PUT /snapshot/create", create.method, create.path)
	}
	assertJSONField(t, create.body, "snapshot_type", `"Full"`)
	assertJSONField(t, create.body, "snapshot_path", `"/snap/state"`)
	assertJSONField(t, create.body, "mem_file_path", `"/snap/mem"`)

	if err := c.LoadSnapshot(ctx, "/snap/state", "/snap/mem", true); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	load := <-got
	if load.method != http.MethodPut || load.path != "/snapshot/load" {
		t.Errorf("load = %s %s, want PUT /snapshot/load", load.method, load.path)
	}
	assertJSONField(t, load.body, "snapshot_path", `"/snap/state"`)
	assertJSONField(t, load.body, "resume_vm", `true`)
	// Guest memory is addressed via the mem_backend object, not the legacy flat
	// mem_file_path field.
	mb, ok := load.body["mem_backend"]
	if !ok {
		t.Fatalf("load body missing mem_backend; got %v", keys(load.body))
	}
	for _, want := range []string{`"backend_type":"File"`, `"backend_path":"/snap/mem"`} {
		if !strings.Contains(string(mb), want) {
			t.Errorf("mem_backend %s missing %s", mb, want)
		}
	}
}

func assertJSONField(t *testing.T, body map[string]json.RawMessage, key, want string) {
	t.Helper()
	got, ok := body[key]
	if !ok {
		t.Errorf("body missing %q; got keys %v", key, keys(body))
		return
	}
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("%s = %s, want %s", key, got, want)
	}
}

func TestDefaultBootArgs(t *testing.T) {
	got := DefaultBootArgs("172.16.0.2", "172.16.0.1")
	// console=ttyS0 is omitted by default to avoid serial I/O VM-exits on boot.
	if strings.Contains(got, "console=ttyS0") {
		t.Errorf("boot args %q should not contain console=ttyS0 by default", got)
	}
	for _, want := range []string{"ip=172.16.0.2::172.16.0.1:", "init=/usr/bin/sbxguest"} {
		if !strings.Contains(got, want) {
			t.Errorf("boot args %q missing %q", got, want)
		}
	}
}

func TestDefaultBootArgsDebugConsole(t *testing.T) {
	t.Setenv("FIRECRACKER_DEBUG_CONSOLE", "1")
	got := DefaultBootArgs("172.16.0.2", "172.16.0.1")
	if !strings.Contains(got, "console=ttyS0") {
		t.Errorf("boot args %q missing console=ttyS0 when FIRECRACKER_DEBUG_CONSOLE=1", got)
	}
}

// TestDialGuestHandshake verifies the CONNECT/OK handshake against a fake
// vsock multiplexing socket.
func TestDialGuestHandshake(t *testing.T) {
	dir := t.TempDir()
	uds := filepath.Join(dir, "vsock.sock")
	ln, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	gotPort := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		gotPort <- strings.TrimSpace(line)
		_, _ = conn.Write([]byte("OK 10000\n"))
		// Echo one line so the caller can confirm the live stream works.
		_, _ = conn.Write([]byte("ready\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := DialGuest(ctx, uds, GuestExecPort)
	if err != nil {
		t.Fatalf("DialGuest: %v", err)
	}
	defer conn.Close()

	select {
	case line := <-gotPort:
		want := "CONNECT " + itoa(int(GuestExecPort))
		if line != want {
			t.Errorf("handshake line = %q, want %q", line, want)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive CONNECT")
	}
}

func TestDialGuestRejectsBadAck(t *testing.T) {
	dir := t.TempDir()
	uds := filepath.Join(dir, "vsock.sock")
	ln, _ := net.Listen("unix", uds)
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("ERR no such port\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := DialGuest(ctx, uds, GuestExecPort); err == nil {
		t.Fatal("expected error on non-OK ack")
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
