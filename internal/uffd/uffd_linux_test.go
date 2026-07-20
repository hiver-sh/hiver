//go:build linux

package uffd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Firecracker-side ioctls. The handler never issues these (Firecracker owns the
// uffd and registers the regions); the test does, to play Firecracker's part.
const (
	testUffdioAPI      = 0xc018aa3f
	testUffdioRegister = 0xc020aa00
	testAPIVersion     = 0xAA
	testRegModeMissing = 1
	testMapHugetlb     = 0x40000
)

type testAPIArg struct{ api, features, ioctls uint64 }
type testRegisterArg struct{ start, len, mode, ioctls uint64 }

// TestServePopulatesRegion drives the handler exactly as Firecracker does:
// create a userfaultfd, register an unpopulated guest-memory region, hand both
// the fd and the mapping list to the handler over its socket, and assert the
// handler fills the region from the memory file.
//
// Covers the parts the raw-ABI probe could not: socket accept, SCM_RIGHTS
// receive, mapping JSON decode, and Serve's populate path. What it still does
// NOT cover is Firecracker's own wire format — the JSON field names here are
// written to match what Firecracker sends, but only a real load proves that.
func TestServePopulatesRegion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags int
		size  int
	}{
		{"anon-4k", unix.MAP_PRIVATE | unix.MAP_ANONYMOUS, 2 << 20},
		{"hugetlb-2m", unix.MAP_PRIVATE | unix.MAP_ANONYMOUS | testMapHugetlb, 2 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst, err := unix.Mmap(-1, 0, tc.size, unix.PROT_READ|unix.PROT_WRITE, tc.flags)
			if err != nil {
				t.Skipf("mmap (%s) unavailable here: %v", tc.name, err)
			}
			defer unix.Munmap(dst)

			// Memory file the handler copies from, filled with a checkable pattern.
			dir := t.TempDir()
			memPath := filepath.Join(dir, "mem.bin")
			want := make([]byte, tc.size)
			for i := range want {
				want[i] = byte(i % 251)
			}
			if err := os.WriteFile(memPath, want, 0o600); err != nil {
				t.Fatalf("write mem file: %v", err)
			}

			uffd, err := newRegisteredUffd(dst)
			if err != nil {
				t.Skipf("userfaultfd unavailable here (needs CAP_SYS_PTRACE): %v", err)
			}
			defer unix.Close(uffd)

			h, err := Listen(filepath.Join(dir, "uffd.sock"), memPath, Options{})
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			defer h.Close()

			served := make(chan error, 1)
			go func() { served <- h.Serve() }()

			mappings := []Mapping{{
				BaseHostVirtAddr: uint64(uintptr(unsafe.Pointer(&dst[0]))),
				Size:             uint64(tc.size),
				Offset:           0,
			}}
			if err := sendHandshake(h.SocketPath(), uffd, mappings); err != nil {
				t.Fatalf("handshake: %v", err)
			}

			// Poll rather than sleep: population is asynchronous in Serve.
			deadline := time.Now().Add(10 * time.Second)
			for {
				if dst[0] == want[0] && dst[tc.size-1] == want[tc.size-1] {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("region not populated within timeout")
				}
				time.Sleep(20 * time.Millisecond)
			}

			for _, off := range []int{0, 4096, tc.size / 2, tc.size - 1} {
				if dst[off] != want[off] {
					t.Fatalf("byte %d: got %d want %d", off, dst[off], want[off])
				}
			}

			h.Close()
			select {
			case err := <-served:
				if err != nil {
					t.Fatalf("Serve returned: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not return after Close")
			}
		})
	}
}

// TestCloseDuringBackgroundPopulate guards the teardown race: with Background
// set, copies outlive the resume, so a VM stopped mid-population must not have
// the copy source unmapped underneath it. Unsynchronised this segfaults in
// sandboxd — pid 1 in a pool pod — taking every sandbox on the node with it,
// which makes it worth a test even though it reproduces probabilistically.
func TestCloseDuringBackgroundPopulate(t *testing.T) {
	const size = 64 << 20 // large enough that population is still running at Close

	for i := 0; i < 20; i++ {
		dst, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE,
			unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil {
			t.Skipf("mmap: %v", err)
		}

		dir := t.TempDir()
		memPath := filepath.Join(dir, "mem.bin")
		if err := os.WriteFile(memPath, make([]byte, size), 0o600); err != nil {
			t.Fatalf("write mem file: %v", err)
		}

		uffd, err := newRegisteredUffd(dst)
		if err != nil {
			unix.Munmap(dst)
			t.Skipf("userfaultfd unavailable: %v", err)
		}

		h, err := Listen(filepath.Join(dir, "uffd.sock"), memPath,
			Options{Background: true, Workers: 4})
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		go h.Serve()
		if err := sendHandshake(h.SocketPath(), uffd, []Mapping{{
			BaseHostVirtAddr: uint64(uintptr(unsafe.Pointer(&dst[0]))),
			Size:             size,
			Offset:           0,
		}}); err != nil {
			t.Fatalf("handshake: %v", err)
		}

		// Close while the copiers are mid-flight. Surviving the loop is the
		// assertion: an unsynchronised unmap crashes the process outright.
		time.Sleep(time.Duration(i) * time.Millisecond)
		h.Close()

		unix.Close(uffd)
		unix.Munmap(dst)
	}
}

// TestListenRejectsBusyPath covers the per-VM socket lifecycle: a stale socket
// is cleared, but a genuinely unusable path still errors rather than silently
// handing Firecracker a path nothing is listening on.
func TestListenRejectsBusyPath(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "uffd.sock")

	h1, err := Listen(sock, filepath.Join(dir, "mem.bin"), Options{})
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	// Stale-socket reuse: a second Listen on the same path must succeed, since a
	// prior VM's leftover socket file would otherwise wedge every later resume.
	h2, err := Listen(sock, filepath.Join(dir, "mem.bin"), Options{})
	if err != nil {
		t.Fatalf("Listen over a stale socket should succeed: %v", err)
	}
	h1.Close()
	h2.Close()

	if _, err := Listen(filepath.Join(dir, "no-such-dir", "uffd.sock"), "", Options{}); err == nil {
		t.Fatal("Listen on an unwritable path should fail")
	}
}

// newRegisteredUffd creates a userfaultfd and registers region for missing-page
// faults — Firecracker's half of the setup.
func newRegisteredUffd(region []byte) (int, error) {
	r0, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK, 0, 0)
	if errno != 0 {
		return -1, fmt.Errorf("userfaultfd: %w", errno)
	}
	fd := int(r0)
	api := testAPIArg{api: testAPIVersion}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), testUffdioAPI, uintptr(unsafe.Pointer(&api))); e != 0 {
		unix.Close(fd)
		return -1, fmt.Errorf("UFFDIO_API: %w", e)
	}
	reg := testRegisterArg{
		start: uint64(uintptr(unsafe.Pointer(&region[0]))),
		len:   uint64(len(region)),
		mode:  testRegModeMissing,
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), testUffdioRegister, uintptr(unsafe.Pointer(&reg))); e != 0 {
		unix.Close(fd)
		return -1, fmt.Errorf("UFFDIO_REGISTER: %w", e)
	}
	return fd, nil
}

// sendHandshake mimics Firecracker: one message carrying the uffd as SCM_RIGHTS
// with the mapping list as the JSON body.
func sendHandshake(sockPath string, uffd int, mappings []Mapping) error {
	c, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.Close()

	body, err := json.Marshal(mappings)
	if err != nil {
		return err
	}
	rights := syscall.UnixRights(uffd)
	if _, _, err := c.(*net.UnixConn).WriteMsgUnix(body, rights, nil); err != nil {
		return fmt.Errorf("WriteMsgUnix: %w", err)
	}
	return nil
}
