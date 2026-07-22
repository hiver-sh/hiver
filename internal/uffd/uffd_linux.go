//go:build linux

package uffd

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// userfaultfd ABI. x/sys/unix exposes only SYS_USERFAULTFD, so the ioctl number
// and struct layout are spelled out here (linux/userfaultfd.h, x86_64).
const (
	// uffdioCopy = _IOWR(UFFDIO, _UFFDIO_COPY, struct uffdio_copy), i.e.
	// (_IOC_READ|_IOC_WRITE)<<30 | sizeof(uffdio_copy)<<16 | 'A'<<8 | 0x03,
	// with UFFDIO == 0xAA and sizeof(struct uffdio_copy) == 40.
	uffdioCopy = 0xc028aa03

	// uffdioWake = _IOR(UFFDIO, _UFFDIO_WAKE, struct uffdio_range), with
	// sizeof(struct uffdio_range) == 16.
	//
	// _IOR, NOT _IOWR (the kernel's uffdio_range has no written-back field). The
	// kernel dispatches on the FULL 32-bit command, so the _IOWR encoding
	// (0xc010aa02) this originally used is simply not UFFDIO_WAKE: every call
	// fell through the ioctl switch to -EINVAL and no thread was ever woken.
	// That made the EEXIST-defensive wake in serveResidual a silent no-op — the
	// "added, no change" result in the huge-pages investigation was measuring a
	// wake that never executed. TestWakeIoctlEncoding fails against the _IOWR
	// encoding and passes against this one.
	uffdioWake = 0x8010aa02

	uffdEventPagefault = 0x12

	// sizeof(struct uffd_msg). The pagefault address sits at offset 16: an 8-byte
	// header (event + reserved), then the union's 8-byte flags field.
	uffdMsgSize     = 32
	uffdMsgAddrOff  = 16
	uffdMsgEventOff = 0
)

// uffdioRangeArg mirrors struct uffdio_range.
type uffdioRangeArg struct {
	start uint64
	len   uint64
}

// uffdioCopyArg mirrors struct uffdio_copy.
type uffdioCopyArg struct {
	dst  uint64
	src  uint64
	len  uint64
	mode uint64
	copy int64
}

// Handler serves guest-memory faults for one microVM. Listen must be called
// before Firecracker's /snapshot/load, since Firecracker connects during it.
type Handler struct {
	sockPath string
	memPath  string
	opts     Options

	ln   net.Listener
	mem  []byte
	memF *os.File

	populateNanos  atomic.Int64
	residualFaults atomic.Int64
	bytesCopied    atomic.Int64
	misaligned     atomic.Int64

	// memMu guards the memory mapping against teardown. Copies run concurrently
	// (Options.Workers) and, with Options.Background, outlive the resume — so a
	// VM torn down mid-population would otherwise munmap the copy source out from
	// under a live UFFDIO_COPY. That is a segfault in sandboxd, which is pid 1 in
	// a pool pod, so it would take down every sandbox on the node rather than the
	// one being stopped. Readers hold it for a single ioctl; Close takes it
	// exclusively before unmapping.
	memMu sync.RWMutex

	mu     sync.Mutex
	closed bool
}

// pageSize is the granularity UFFDIO_COPY works at for this guest: the hugetlb
// page size when the snapshot is huge-page backed, otherwise the host page size.
func (h *Handler) pageSize() uint64 {
	if h.opts.HugePageSize > 0 {
		return h.opts.HugePageSize
	}
	return uint64(os.Getpagesize())
}

// copied interprets uffdio_copy.copy after a FAILED ioctl and reports how many
// bytes actually landed, if any.
//
// The field is overloaded, and getting this wrong is silent and catastrophic.
// mfill_atomic returns `copied ? copied : err`, and the kernel writes that
// straight into uffdio_copy.copy — so the field is a byte count only when
// POSITIVE. On zero progress it holds NEGATIVE ERRNO: an EEXIST that copied
// nothing reports -17, not 0.
//
// Reading that as an unsigned count yields 2^64-17, which is what the original
// code did. Every consequence of that is invisible at the call site: the range
// looks fully consumed, so population returns having filled nothing, and the
// guest is left demand-paging memory the copier believes it already wrote.
//
// So: treat only positive values as progress. The alignment check is a standing
// assertion — a positive count under hugetlbfs is always a 2MiB multiple, and if
// that ever stops being true, advancing by it would shift every subsequent copy
// and write snapshot bytes to the wrong guest addresses.
func (h *Handler) copied(reported int64, cause string) (uint64, bool) {
	if reported <= 0 {
		return 0, false
	}
	page := h.pageSize()
	aligned := uint64(reported) &^ (page - 1)
	if aligned != uint64(reported) {
		h.misaligned.Add(1)
		log.Printf("uffd: %s reported %d bytes copied, not a multiple of page size %d — using %d",
			cause, reported, page, aligned)
	}
	if aligned == 0 {
		return 0, false
	}
	h.bytesCopied.Add(int64(aligned))
	return aligned, true
}

// Stats returns population counters; safe to call while serving.
func (h *Handler) Stats() Stats {
	return Stats{
		PopulateNanos:  h.populateNanos.Load(),
		ResidualFaults: h.residualFaults.Load(),
		BytesCopied:    h.bytesCopied.Load(),
		Misaligned:     h.misaligned.Load(),
	}
}

// Listen creates the handler socket at sockPath and starts listening. The
// caller passes sockPath to Firecracker as the Uffd backend path.
func Listen(sockPath, memPath string, opts Options) (*Handler, error) {
	// A stale socket from a previous VM under the same path would make Bind
	// fail; the path is per-VM and owned by us, so removing it is safe.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	return &Handler{sockPath: sockPath, memPath: memPath, opts: opts, ln: ln}, nil
}

// SocketPath is the path to hand Firecracker as the Uffd backend_path.
func (h *Handler) SocketPath() string { return h.sockPath }

// Serve accepts Firecracker's connection, populates guest memory from the
// snapshot file, then keeps serving any residual faults until Close. It blocks,
// so callers run it in a goroutine started before /snapshot/load.
func (h *Handler) Serve() error {
	conn, err := h.ln.Accept()
	if err != nil {
		if h.isClosed() {
			return nil // Close raced the accept; not an error
		}
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()

	uffd, mappings, err := recvHandshake(conn.(*net.UnixConn))
	if err != nil {
		return fmt.Errorf("uffd handshake: %w", err)
	}
	defer unix.Close(uffd)

	if err := h.openMem(); err != nil {
		return err
	}

	// Populate guest memory so the guest resumes onto memory that is already
	// present instead of taking a fault per page. Mandatory for hugetlbfs, where
	// a partially populated VMA cannot be filled a 4KiB page at a time.
	//
	// Background hands control back to Firecracker immediately and copies while
	// the guest runs, so the copy is off the resume critical path; the fault loop
	// below covers anything the guest reaches first.
	populate := func() {
		start := time.Now()
		for _, m := range mappings {
			if err := h.copyRegion(uffd, m); err != nil {
				log.Printf("uffd: populate region at %#x: %v", m.BaseHostVirtAddr, err)
				return
			}
		}
		h.populateNanos.Store(int64(time.Since(start)))
	}
	var populating sync.WaitGroup
	if h.opts.Background {
		populating.Add(1)
		go func() { defer populating.Done(); populate() }()
	} else {
		populate()
	}

	// Serve faults for anything not yet populated. With Background this is a live
	// race against the copier; without it, a fault here means a region we did not
	// cover, and serving it beats wedging the guest.
	h.serveResidual(uffd, mappings)

	// The copier must finish before the deferred Close(uffd) above: closing the
	// fd out from under an in-flight UFFDIO_COPY both fails the copy and leaves
	// Firecracker's faults unanswered, which hangs /snapshot/load.
	populating.Wait()
	return nil
}

// openMem maps the snapshot memory file read-only as the copy source.
func (h *Handler) openMem() error {
	f, err := os.Open(h.memPath)
	if err != nil {
		return fmt.Errorf("open mem file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat mem file: %w", err)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		f.Close()
		return fmt.Errorf("mmap mem file: %w", err)
	}
	h.memMu.Lock()
	h.memF, h.mem = f, data
	h.memMu.Unlock()
	return nil
}

// copyRegion fills one mapping from the memory file, splitting the work across
// Workers goroutines. A single UFFDIO_COPY stream does not saturate memory
// bandwidth, so chunking is what makes population fast enough to stay ahead of
// a running guest.
func (h *Handler) copyRegion(uffd int, m Mapping) error {
	end := m.Offset + m.Size
	if end > uint64(len(h.mem)) {
		return fmt.Errorf("region [%d,%d) exceeds mem file (%d bytes)", m.Offset, end, len(h.mem))
	}

	workers := h.opts.Workers
	if workers < 1 {
		workers = 1
	}

	// Copy in fixed slabs rather than one ioctl per region. UFFDIO_COPY reports
	// EEXIST for the WHOLE call if any page in the destination range is already
	// mapped, with no indication of how much is left — so a region-sized call
	// that races a single guest fault would abandon everything behind it. At slab
	// granularity an EEXIST skips only that slab. 2MiB also keeps every call
	// huge-page aligned, which hugetlbfs requires and 4KiB pages tolerate.
	const slab = 2 << 20

	slabs := make(chan uint64, workers*2)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for off := range slabs {
				size := uint64(slab)
				if off+size > m.Size {
					size = m.Size - off
				}
				if err := h.copyRange(uffd, m.BaseHostVirtAddr+off, m.Offset+off, size); err != nil {
					errs[w] = err
					return
				}
			}
		}(w)
	}
	for off := uint64(0); off < m.Size; off += slab {
		slabs <- off
	}
	close(slabs)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// copyRange fills [dst, dst+size) from the memory file at srcOff.
//
// Two errnos are expected rather than fatal, and both only show up once the
// copier races a running guest (i.e. Options.Background):
//
//   - EEXIST: the guest faulted this range and it was already served.
//   - EAGAIN: contention on the address space, possibly after a partial copy.
//
// In both cases uffdio_copy.copy carries either the bytes copied or a negative
// errno — see copied(), which is where that distinction is enforced.
func (h *Handler) copyRange(uffd int, dst, srcOff, size uint64) error {
	// Held across the ioctl so Close cannot unmap the source mid-copy.
	h.memMu.RLock()
	defer h.memMu.RUnlock()
	if h.mem == nil {
		return nil // torn down while queued; nothing left to populate
	}

	// Bounded so a pathological retry loop fails loudly instead of spinning.
	const maxAttempts = 1024
	for attempt := 0; size > 0 && attempt < maxAttempts; attempt++ {
		arg := uffdioCopyArg{
			dst: dst,
			src: uint64(uintptr(unsafe.Pointer(&h.mem[srcOff]))),
			len: size,
		}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioCopy, uintptr(unsafe.Pointer(&arg)))
		switch errno {
		case 0:
			h.bytesCopied.Add(int64(size))
			return nil
		case unix.EEXIST:
			// A page inside this range is already mapped — the guest reached it
			// first. The kernel fills up to that page, so skip past it and keep
			// going.
			//
			// Returning here instead (the original behaviour) abandons the rest of
			// the range: the copier reports success having filled nothing past the
			// present page, and the guest silently demand-pages the remainder.
			// TestPopulatesAroundAlreadyPresentPage covers this and fails against
			// the old arithmetic, abandoning ~2MiB from the first present page on.
			//
			// Note this is NOT the cause of the intermittent huge-pages turn
			// failures — those persist with this fixed. Do not read this comment as
			// an explanation for that; it remains open.
			skip, ok := h.copied(arg.copy, "EEXIST")
			if !ok {
				// Nothing copied: the already-present page is at the very start of
				// the range, so step over exactly one page to get past it.
				skip = h.pageSize()
			}
			if skip >= size {
				return nil
			}
			dst += skip
			srcOff += skip
			size -= skip
			continue
		case unix.EAGAIN:
			done, ok := h.copied(arg.copy, "EAGAIN")
			if !ok {
				// No bytes moved — retry the same range rather than advancing.
				continue
			}
			if done > size {
				return fmt.Errorf("UFFDIO_COPY reported %d copied of %d", done, size)
			}
			dst += done
			srcOff += done
			size -= done
			continue
		default:
			return fmt.Errorf("UFFDIO_COPY: %w", errno)
		}
	}
	if size > 0 {
		return fmt.Errorf("UFFDIO_COPY: gave up with %d bytes unfilled", size)
	}
	return nil
}

// serveResidual answers any fault that arrives after population by copying the
// page's whole containing region. Regions are copied wholesale, so one fault
// resolves everything behind it.
// wake releases any threads parked on [start, start+len). start and len must be
// page-aligned and inside a registered region, or the kernel returns EINVAL.
func wake(uffd int, start, len uint64) error {
	arg := uffdioRangeArg{start: start, len: len}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioWake,
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return errno
	}
	return nil
}

func (h *Handler) serveResidual(uffd int, mappings []Mapping) {
	buf := make([]byte, uffdMsgSize)
	for {
		if h.isClosed() {
			return
		}
		// Firecracker hands us a non-blocking uffd, so a bare Read returns EAGAIN
		// the instant no fault is pending — poll instead, with a timeout short
		// enough to notice Close promptly.
		fds := []unix.PollFd{{Fd: int32(uffd), Events: unix.POLLIN}}
		switch n, err := unix.Poll(fds, 200); {
		case err == unix.EINTR:
			continue
		case err != nil:
			return
		case n == 0:
			continue // timeout: re-check closed, keep waiting
		}

		n, err := unix.Read(uffd, buf)
		if err == unix.EAGAIN {
			continue
		}
		if err != nil || n < uffdMsgSize {
			return // closed, or a short read we cannot interpret
		}
		if buf[uffdMsgEventOff] != uffdEventPagefault {
			continue
		}
		addr := *(*uint64)(unsafe.Pointer(&buf[uffdMsgAddrOff]))
		h.residualFaults.Add(1)
		for _, m := range mappings {
			if addr >= m.BaseHostVirtAddr && addr < m.BaseHostVirtAddr+m.Size {
				// Start at the faulting PAGE, not at the slab boundary. Anything
				// before it may already be mapped (that is what the copier was
				// doing), and starting there would return EEXIST before ever
				// reaching the address the guest is blocked on.
				page := h.pageSize()
				rel := (addr - m.BaseHostVirtAddr) &^ (page - 1)
				// Serve up to a slab's worth from there, so one fault also fills
				// what follows rather than trapping again immediately.
				const slab = 2 << 20
				size := uint64(slab)
				if size < page {
					size = page
				}
				if rel+size > m.Size {
					size = m.Size - rel
				}
				if err := h.copyRange(uffd, m.BaseHostVirtAddr+rel, m.Offset+rel, size); err != nil {
					log.Printf("uffd: residual fault at %#x: %v", addr, err)
				}
				// Wake the faulting page explicitly.
				//
				// UFFDIO_COPY only wakes the range it actually copied, so a fault on
				// a page that is ALREADY mapped gets EEXIST, copies nothing, and
				// wakes nobody. The faulting thread parks in handle_userfault and
				// never re-checks the PTE, so it blocks forever while the rest of
				// the guest keeps running.
				//
				// That can happen when two faults race on one page: the first is
				// served, the second was already queued and finds the page present.
				//
				// This is defensive — a redundant wake is a no-op, and one ioctl per
				// residual fault is not worth the risk of a permanently parked guest
				// thread. It is NOT a demonstrated fix for anything: the race above
				// could not be reproduced in a test, since the copier's own
				// UFFDIO_COPY already wakes the range it fills.
				if err := wake(uffd, m.BaseHostVirtAddr+rel, page); err != nil {
					log.Printf("uffd: wake %#x: %v", addr, err)
				}
				break
			}
		}
	}
}

// Close stops the listener and releases the memory mapping.
func (h *Handler) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()

	if h.ln != nil {
		h.ln.Close()
	}
	// Exclusive: blocks until every in-flight copy has finished, so the source is
	// never unmapped under a running UFFDIO_COPY.
	h.memMu.Lock()
	if h.mem != nil {
		unix.Munmap(h.mem)
		h.mem = nil
	}
	h.memMu.Unlock()
	if h.memF != nil {
		h.memF.Close()
		h.memF = nil
	}
	_ = os.Remove(h.sockPath)
	return nil
}

func (h *Handler) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// recvHandshake reads Firecracker's single setup message: the guest's
// userfaultfd as an SCM_RIGHTS control message, with the mapping list as the
// JSON payload.
func recvHandshake(conn *net.UnixConn) (int, []Mapping, error) {
	buf := make([]byte, 8192)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, nil, fmt.Errorf("read: %w", err)
	}

	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, nil, fmt.Errorf("parse control message: %w", err)
	}
	if len(scms) == 0 {
		return -1, nil, fmt.Errorf("no control message (expected the userfaultfd)")
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, nil, fmt.Errorf("parse rights: %w", err)
	}
	if len(fds) == 0 {
		return -1, nil, fmt.Errorf("no fd in control message")
	}

	var mappings []Mapping
	if err := json.Unmarshal(buf[:n], &mappings); err != nil {
		unix.Close(fds[0])
		return -1, nil, fmt.Errorf("decode mappings %q: %w", string(buf[:n]), err)
	}
	if len(mappings) == 0 {
		unix.Close(fds[0])
		return -1, nil, fmt.Errorf("no mappings in handshake")
	}
	return fds[0], mappings, nil
}
