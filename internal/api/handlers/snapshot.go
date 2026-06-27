package handlers

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/snapshot"
)

// Snapshot captures a snapshot of the running sandbox now, without stopping it.
// The body selects which parts to capture: vm (full microVM state, a no-op on a
// container) and/or files (the writable filesystem as a tarball). Each part is
// keyed and reported independently.
func (s *Sandbox) Snapshot(c *gin.Context) {
	var req gen.Snapshot
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "invalid snapshot request: " + err.Error()})
		return
	}
	if req.Vm == nil && req.Files == nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "snapshot request must select at least one of vm, files"})
		return
	}

	// Readiness is guaranteed by /v1/ping before any client action; a snapshot of
	// a not-yet-running workload is meaningless, so require it up.
	if !s.Ready() {
		c.JSON(http.StatusServiceUnavailable, gen.Error{Error: "sandbox not ready"})
		return
	}
	s.ResetLifetime()

	ctx := c.Request.Context()

	// Resolve where each part is written. The VM snapshot always lands in the
	// host's local snapshot dir (it is large, mmap-resumed, and node-local). The
	// files tarball follows its mount override (a FUSE drive) when set, else the
	// local dir.
	vmDir := ""
	if req.Vm != nil {
		if s.snapshotDir == "" {
			c.JSON(http.StatusBadRequest, gen.Error{Error: "vm snapshot requested but no local snapshot dir is configured"})
			return
		}
		vmDir = snapshot.VMSnapshotDir(s.snapshotDir, req.Vm.Key)
	}
	filesDst, filesDir := "", ""
	var include []string
	if req.Files != nil {
		filesDir = s.snapshotDir
		if req.Files.Mount != nil && *req.Files.Mount != "" {
			filesDir = *req.Files.Mount
		}
		if filesDir == "" {
			c.JSON(http.StatusBadRequest, gen.Error{Error: "files snapshot requested but no snapshot dir (or files.mount) is configured"})
			return
		}
		filesDst = snapshot.SnapshotPath(filesDir, req.Files.Key)
		if req.Files.Include != nil {
			include = *req.Files.Include
		}
	}

	// Flush the workload's filesystem so the capture reads durable state (microvm:
	// syncs guest page cache to the virtio block device; container: a no-op).
	if err := s.iso.FlushAgent(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: "flush workload: " + err.Error()})
		return
	}

	vmCaptured, err := s.iso.SnapshotLive(ctx, vmDir, filesDst, include)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: "capture snapshot: " + err.Error()})
		return
	}

	var result gen.SnapshotResult
	if req.Vm != nil {
		part := gen.SnapshotPartResult{Captured: vmCaptured, Key: req.Vm.Key}
		if vmCaptured {
			// Unlike the files tarball, a VM snapshot is a directory
			// (snapshot.bin + mem.bin + overlay.ext4); report the sum of its
			// parts so the client sees the on-disk size of the capture.
			if n, err := dirSize(vmDir); err == nil {
				part.Bytes = &n
			}
		} else {
			reason := "vm snapshots require microvm isolation; the active backend has no VM state"
			part.Reason = &reason
		}
		result.Vm = &part
	}
	if req.Files != nil {
		part := gen.SnapshotPartResult{Captured: true, Key: req.Files.Key}
		if fi, statErr := os.Stat(filesDst); statErr == nil {
			n := fi.Size()
			part.Bytes = &n
		}
		result.Files = &part
	}
	c.JSON(http.StatusOK, result)
}

// dirSize sums the sizes of all regular files under dir, walking subdirectories.
// Used to report the on-disk size of a VM snapshot, which spans several files.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
