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

	uffdEventPagefault = 0x12

	// sizeof(struct uffd_msg). The pagefault address sits at offset 16: an 8-byte
	// header (event + reserved), then the union's 8-byte flags field.
	uffdMsgSize     = 32
	uffdMsgAddrOff  = 16
	uffdMsgEventOff = 0
)

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

// Stats returns population counters; safe to call while serving.
func (h *Handler) Stats() Stats {
	return Stats{
		PopulateNanos:  h.populateNanos.Load(),
		ResidualFaults: h.residualFaults.Load(),
		BytesCopied:    h.bytesCopied.Load(),
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
//   - EAGAIN: a PARTIAL copy. The kernel sets uffdio_copy.copy to the bytes it
//     managed before contending on the address space, and expects the caller to
//     resume from there. Treating it as fatal aborts population mid-region and
//     silently leaves the guest demand-paging the remainder.
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
			return nil
		case unix.EAGAIN:
			if arg.copy > 0 {
				done := uint64(arg.copy)
				if done > size {
					return fmt.Errorf("UFFDIO_COPY reported %d copied of %d", done, size)
				}
				h.bytesCopied.Add(arg.copy)
				dst += done
				srcOff += done
				size -= done
			}
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
				// Serve just the faulting 2MiB slab rather than the whole region:
				// the copier may still be working through it, and a huge-page
				// aligned slab is the smallest unit UFFDIO_COPY accepts on hugetlbfs.
				const slab = 2 << 20
				rel := (addr - m.BaseHostVirtAddr) &^ uint64(slab-1)
				size := uint64(slab)
				if rel+size > m.Size {
					size = m.Size - rel
				}
				if err := h.copyRange(uffd, m.BaseHostVirtAddr+rel, m.Offset+rel, size); err != nil {
					log.Printf("uffd: residual fault at %#x: %v", addr, err)
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
