// Package uffd implements the handler side of Firecracker's userfaultfd memory
// backend: it listens on a Unix socket, receives the guest's userfaultfd plus a
// description of the guest-memory mappings, and fills those mappings from the
// snapshot's memory file.
package uffd

// Mapping is one guest-memory region, as described by Firecracker over the
// handler socket. Offset is the region's start within the snapshot memory file;
// BaseHostVirtAddr is where Firecracker mapped it in its own address space,
// which is the address the userfaultfd operates on.
type Mapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
}

// Options tunes how guest memory is populated.
type Options struct {
	// Background returns control to Firecracker as soon as the handshake is done
	// and copies in the background, so the guest resumes immediately instead of
	// waiting for the whole image. Faults that outrun the copier are served on
	// demand. Without it the copy completes before the guest runs, which is
	// simpler but puts the full copy on the resume critical path.
	Background bool
	// Workers splits each region across N goroutines. 0 or 1 copies serially.
	// A single UFFDIO_COPY stream does not saturate memory bandwidth.
	Workers int
	// HugePageSize is the guest's page size in bytes when its memory is backed by
	// hugetlbfs (e.g. 2MiB), or 0 for ordinary 4KiB pages. UFFDIO_COPY operates
	// at page granularity, so this is how far the handler must step when it lands
	// on a page the guest already faulted in.
	HugePageSize uint64
}

// Stats reports what population actually cost, for tuning Options.
type Stats struct {
	// PopulateNanos is wall time from handshake to the last region copied.
	PopulateNanos int64
	// ResidualFaults counts faults served on demand — i.e. the guest reached a
	// page before the copier did. Zero means population always won the race.
	ResidualFaults int64
	// BytesCopied is the total moved into guest memory.
	BytesCopied int64
	// Misaligned counts kernel-reported byte counts that were not a multiple of
	// the guest page size. Nonzero means the guard in advance() fired, i.e. a
	// naive resume-at-reported-offset would have written snapshot bytes to the
	// wrong guest addresses.
	Misaligned int64
}
