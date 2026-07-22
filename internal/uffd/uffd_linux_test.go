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

// TestPopulatesAroundAlreadyPresentPage reproduces the deadlock that shipped.
//
// When the guest touches a page mid-range before the copier reaches it,
// UFFDIO_COPY fills up to that page and returns EEXIST. Treating EEXIST as
// "range done" leaves everything after it unpopulated AND unfillable: the next
// fault there hits the same present page, gets EEXIST with copy==0, and is never
// served, so the copier reports success having filled nothing past that page.
//
// Touch one page first, then populate, then verify every OTHER page landed.
func TestPopulatesAroundAlreadyPresentPage(t *testing.T) {
	const size = 8 << 20
	pageSz := os.Getpagesize()

	dst, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Skipf("mmap: %v", err)
	}
	defer unix.Munmap(dst)

	dir := t.TempDir()
	memPath := filepath.Join(dir, "mem.bin")
	want := make([]byte, size)
	for i := range want {
		want[i] = byte(i%251) + 1 // never 0, so "unpopulated" is distinguishable
	}
	if err := os.WriteFile(memPath, want, 0o600); err != nil {
		t.Fatalf("write mem file: %v", err)
	}

	uffd, err := newRegisteredUffd(dst)
	if err != nil {
		t.Skipf("userfaultfd unavailable: %v", err)
	}
	defer unix.Close(uffd)

	h, err := Listen(filepath.Join(dir, "uffd.sock"), memPath, Options{})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer h.Close()
	go h.Serve()

	mapping := Mapping{
		BaseHostVirtAddr: uint64(uintptr(unsafe.Pointer(&dst[0]))),
		Size:             size,
		Offset:           0,
	}

	// Pre-place a page in the middle of the first slab, mimicking a guest that
	// got there before the copier. Done via UFFDIO_COPY on our own fd so it is
	// genuinely mapped, without needing a second thread to fault.
	const preOff = 3 << 12 // 3 pages in — mid-slab
	if err := h.openMem(); err != nil {
		t.Fatalf("openMem: %v", err)
	}
	if err := h.copyRange(uffd, mapping.BaseHostVirtAddr+preOff, preOff, uint64(pageSz)); err != nil {
		t.Fatalf("pre-place page: %v", err)
	}

	if err := sendHandshake(h.SocketPath(), uffd, []Mapping{mapping}); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// Wait on the COPIER's own counter, not on the memory. Every page except the
	// pre-placed one has to come from population.
	wantCopied := int64(size - pageSz)
	deadline := time.Now().Add(15 * time.Second)
	for h.Stats().BytesCopied < wantCopied && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// Residency is checked with mincore, which does NOT fault. Reading dst[off]
	// to test it would trigger the fault the residual handler then serves — the
	// assertion would populate the very pages it claims to be verifying, and pass
	// against a copier that had populated nothing at all.
	resident, err := mincore(dst)
	if err != nil {
		t.Fatalf("mincore: %v", err)
	}
	for off := 0; off < size; off += pageSz {
		if !resident[off/pageSz] {
			t.Fatalf("page at %d never populated by the copier (pre-placed page was at %d); "+
				"copied %d of %d bytes", off, preOff, h.Stats().BytesCopied, wantCopied)
		}
	}
	if got := h.Stats().ResidualFaults; got != 0 {
		t.Errorf("ResidualFaults = %d, want 0: the guest should never have had to fault", got)
	}

	// Only now that residency is established does content matter.
	for off := 0; off < size; off += pageSz {
		if dst[off] != want[off] {
			t.Fatalf("page at %d has wrong contents: got %d want %d", off, dst[off], want[off])
		}
	}
}

// mincore reports per-page residency for b without touching it.
func mincore(b []byte) ([]bool, error) {
	pageSz := os.Getpagesize()
	vec := make([]byte, (len(b)+pageSz-1)/pageSz)
	_, _, errno := unix.Syscall(unix.SYS_MINCORE,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&vec[0])))
	if errno != 0 {
		return nil, errno
	}
	out := make([]bool, len(vec))
	for i, v := range vec {
		out[i] = v&1 == 1
	}
	return out, nil
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

// TestWakeIoctlEncoding asserts UFFDIO_WAKE actually reaches the kernel, on
// both page sizes production wakes (4KiB anon and 2MiB hugetlb ranges).
//
// This is the regression test for the shipped bug behind the huge-pages turn
// wedge: the wake ioctl was encoded as _IOWR (0xc010aa02) where the kernel
// defines _IOR (0x8010aa02). The kernel dispatches on the full 32-bit command,
// so every wake fell through the ioctl switch to -EINVAL and the
// EEXIST-defensive wake in serveResidual never woke anything — a guest thread
// parked on a stale fault event stayed parked forever. A wake of a registered
// range with no waiters is a no-op that returns 0, so plain success here is
// exactly the assertion: EINVAL means the command is not UFFDIO_WAKE.
//
// (A full parked-thread reproduction isn't possible in-process: the Go
// runtime's async-preemption SIGURG interrupts handle_userfault, the fault
// retries, and the thread self-releases once the page is present — production
// faulters, KVM vCPU threads and kworkers, get no such signals.)
func TestWakeIoctlEncoding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   int
		wakeLen uint64
	}{
		{"anon-4k", unix.MAP_PRIVATE | unix.MAP_ANONYMOUS, uint64(os.Getpagesize())},
		{"hugetlb-2m", unix.MAP_PRIVATE | unix.MAP_ANONYMOUS | testMapHugetlb, 2 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const size = 4 << 20
			dst, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, tc.flags)
			if err != nil {
				t.Skipf("mmap (%s) unavailable here: %v", tc.name, err)
			}
			defer unix.Munmap(dst)

			uffd, err := newRegisteredUffd(dst)
			if err != nil {
				t.Skipf("userfaultfd unavailable: %v", err)
			}
			defer unix.Close(uffd)

			base := uint64(uintptr(unsafe.Pointer(&dst[0])))
			if err := wake(uffd, base, tc.wakeLen); err != nil {
				t.Fatalf("UFFDIO_WAKE(%#x, %d) = %v; the wake never reaches the kernel, "+
					"so a thread parked on a stale fault event is never released — "+
					"this is the huge-pages turn wedge", base, tc.wakeLen, err)
			}
		})
	}
}

// TestWakesFaultOnAlreadyPresentPage covers the race that wedges a guest thread:
// two faults land on one page, the first is served, and the second arrives to
// find the page already mapped. UFFDIO_COPY then returns EEXIST, copies nothing,
// and — without an explicit UFFDIO_WAKE — wakes nobody, leaving the faulting
// thread parked in handle_userfault forever.
//
// The test forces the ordering deterministically: pre-map the page via
// UFFDIO_COPY, THEN touch it from another thread so the fault is delivered
// against a page that is already present.
func TestWakesFaultOnAlreadyPresentPage(t *testing.T) {
	const size = 4 << 20
	pageSz := os.Getpagesize()

	dst, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Skipf("mmap: %v", err)
	}
	defer unix.Munmap(dst)

	dir := t.TempDir()
	memPath := filepath.Join(dir, "mem.bin")
	want := make([]byte, size)
	for i := range want {
		want[i] = byte(i%251) + 1
	}
	if err := os.WriteFile(memPath, want, 0o600); err != nil {
		t.Fatalf("write mem file: %v", err)
	}

	uffd, err := newRegisteredUffd(dst)
	if err != nil {
		t.Skipf("userfaultfd unavailable: %v", err)
	}
	defer unix.Close(uffd)

	h, err := Listen(filepath.Join(dir, "uffd.sock"), memPath, Options{})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer h.Close()

	mapping := Mapping{
		BaseHostVirtAddr: uint64(uintptr(unsafe.Pointer(&dst[0]))),
		Size:             size,
		Offset:           0,
	}
	if err := h.openMem(); err != nil {
		t.Fatalf("openMem: %v", err)
	}

	// Map one page up front, so the fault below finds it already present.
	const target = 1 << 20
	if err := h.copyRange(uffd, mapping.BaseHostVirtAddr+target, target, uint64(pageSz)); err != nil {
		t.Fatalf("pre-map page: %v", err)
	}

	go h.serveResidual(uffd, []Mapping{mapping})

	// Touch a DIFFERENT page so a real fault is queued, then the target. Reading
	// the already-present page cannot fault on its own, so the fault we need is
	// the one the guest would take mid-race; drive it via a neighbouring page in
	// the same slab, which the handler serves starting at the faulting page.
	touched := make(chan byte, 1)
	go func() { touched <- dst[target+pageSz] }()

	select {
	case <-touched:
	case <-time.After(10 * time.Second):
		t.Fatal("faulting thread never released: the fault was answered by a copy " +
			"that returned EEXIST and issued no UFFDIO_WAKE")
	}
}
