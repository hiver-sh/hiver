package handlers

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/spec"
)

// resolveMount attributes an agent-visible path to the configured mount that
// owns it, returning the mount point, its backend, and whether a mount matched.
// Longest prefix wins so a nested mount beats its parent. ok is false when the
// path falls outside every declared mount — callers skip fs events for those,
// since the stream describes I/O against the sandbox's mounted filesystems.
func (h *Sandbox) resolveMount(cfg gen.SandboxConfig, cleaned string) (mount string, backend gen.Backend, ok bool) {
	for _, f := range cfg.Fs {
		base := fsBase(f)
		m := strings.TrimRight(base.Mount, "/")
		if m == "" {
			continue
		}
		if (cleaned == m || strings.HasPrefix(cleaned, m+"/")) && len(m) >= len(mount) {
			mount, backend, ok = m, base.Backend, true
		}
	}
	return mount, backend, ok
}

// emitFSEvent publishes the request phase of an fs.request/fs.response pair for
// a file API operation and returns a closure that publishes the paired
// response. The file API is a privileged control surface that bypasses the
// FUSE layer (and its ACLs), so writes/reads made through it never cross
// sbxfuse and would otherwise be invisible on GET /v1/events — unlike workload
// I/O, which the sbxfuse audit translator surfaces. Emitting here gives the
// event stream parity: access is always "allowed" (the API is trusted), and
// the response carries the backend + wall-clock duration like the FUSE path.
//
// Only paths under a configured mount are surfaced — the stream describes I/O
// against the sandbox's mounted filesystems, so an operation outside every
// declared mount is a no-op (returns a no-op response closure).
func (h *Sandbox) emitFSEvent(cfg gen.SandboxConfig, op gen.FSRequestEventOperation, cleaned string) func(err error) {
	if h.broker == nil {
		return func(error) {}
	}
	mount, backend, ok := h.resolveMount(cfg, cleaned)
	if !ok {
		return func(error) {}
	}
	start := time.Now()
	reqID := h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromFSRequestEvent(gen.FSRequestEvent{
			Id:        int(id),
			Timestamp: ts,
			Access:    gen.FSRequestEventAccessAllowed,
			Mount:     mount,
			Path:      cleaned,
			Operation: op,
		})
		return ev
	})
	return func(err error) {
		h.broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
			out := gen.FSResponseEvent{
				Id:         int(id),
				Timestamp:  ts,
				RequestId:  int(reqID),
				Backend:    backend,
				DurationMs: int(time.Since(start) / time.Millisecond),
			}
			if err != nil {
				s := err.Error()
				out.Error = &s
			}
			var ev gen.SandboxEvent
			_ = ev.FromFSResponseEvent(out)
			return ev
		})
	}
}

// mountRoutes returns the configured mounts and whether each is remote-backed,
// which the backend's FileBridge uses to route an agent path to its backing
// store (local backend dir vs. the FUSE mount point for remote mounts).
func (h *Sandbox) mountRoutes(cfg gen.SandboxConfig) []isolation.MountRoute {
	out := make([]isolation.MountRoute, 0, len(cfg.Fs))
	for _, f := range cfg.Fs {
		base := fsBase(f)
		out = append(out, isolation.MountRoute{
			Mount:  base.Mount,
			Remote: spec.Backend(base.Backend).IsRemote(),
		})
	}
	return out
}

// UploadFile writes the request body to the agent-visible `path` under one of
// the configured FUSE mounts. `path` is the full destination path (e.g.
// `/workspace/data.csv`); the file lands at `<dir>/<basename>` where those are
// the directory and basename of `path`.
//
// The write goes through the backend's FileBridge, which bypasses the FUSE
// layer's per-mount ACLs — the API is a higher-privilege control surface than
// the workload, so operators seeding inputs shouldn't have to grant the agent
// rw on the same path.
func (h *Sandbox) UploadFile(c *gin.Context, path string) {
	if path == "" || path == "/" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "missing path"})
		return
	}
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must be absolute"})
		return
	}
	dir := filepath.Dir(cleaned)
	name := filepath.Base(cleaned)
	if name == "." || name == "/" || name == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must reference a file"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	done := h.emitFSEvent(cfg, gen.Write, cleaned)
	n, err := h.iso.Files().Save(dir, name, h.mountRoutes(cfg), c.Request.Body)
	done(err)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": cleaned, "bytes": n})
}

// ListDirectory returns the immediate children of a directory, served by the
// backend's FileBridge. Local-backend mounts read the host backend dir directly
// (bypassing sbxfuse ACLs); remote-backed mounts read the FUSE mount point so
// already-flushed files the oplog evicted from the write buffer stay visible.
func (h *Sandbox) ListDirectory(c *gin.Context, params gen.ListDirectoryParams) {
	path := params.Path
	if path == "" {
		path = "/"
	}
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	done := h.emitFSEvent(cfg, gen.Read, cleaned)
	entries, err := h.iso.Files().List(cleaned, h.mountRoutes(cfg))
	done(err)
	if err != nil {
		c.JSON(fileErrStatus(err), gen.Error{Error: err.Error()})
		return
	}

	type dirEntry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	result := make([]dirEntry, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		seen[e.Name] = true
		result = append(result, dirEntry{
			Name:  e.Name,
			Path:  filepath.Join(cleaned, e.Name),
			IsDir: e.IsDir,
			Size:  e.Size,
		})
	}

	// Surface configured workspace mounts that sit directly under this directory.
	// A mount over a lower-image directory leaves no entry in the overlay upper
	// layer (which the file API reads for non-workspace paths), so without this a
	// listing of the mount's parent — e.g. "/" — would never show "workspace" and
	// the mount would be undiscoverable. The mount's own contents are still served
	// live when listed directly (resolveFile routes it to the 9p mount).
	for _, m := range h.mountRoutes(cfg) {
		mount := strings.TrimRight(m.Mount, "/")
		if mount == "" || filepath.Dir(mount) != cleaned {
			continue
		}
		if name := filepath.Base(mount); !seen[name] {
			seen[name] = true
			result = append(result, dirEntry{Name: name, Path: mount, IsDir: true})
		}
	}
	c.JSON(http.StatusOK, gin.H{"entries": result})
}

// GetFile streams a file from the sandbox filesystem via the backend's
// FileBridge, bypassing sbxfuse ACLs.
func (h *Sandbox) GetFile(c *gin.Context, path string) {
	if path == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "missing path"})
		return
	}
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	done := h.emitFSEvent(cfg, gen.Read, cleaned)
	rc, size, err := h.iso.Files().Open(cleaned, h.mountRoutes(cfg))
	done(err)
	if err != nil {
		c.JSON(fileErrStatus(err), gen.Error{Error: err.Error()})
		return
	}
	defer rc.Close()

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(cleaned)))
	c.Header("Content-Length", strconv.FormatInt(size, 10))
	c.DataFromReader(http.StatusOK, size, "application/octet-stream", rc, nil)
}

// DeleteFile removes a file or empty directory at the given agent-visible path.
func (h *Sandbox) DeleteFile(c *gin.Context, path string) {
	if path == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "missing path"})
		return
	}
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	done := h.emitFSEvent(cfg, gen.Write, cleaned)
	err = h.iso.Files().Delete(cleaned, h.mountRoutes(cfg))
	done(err)
	if err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			c.JSON(http.StatusBadRequest, gen.Error{Error: "directory not empty"})
			return
		}
		c.JSON(fileErrStatus(err), gen.Error{Error: err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// fileErrStatus maps a FileBridge error to an HTTP status, surfacing
// not-found as 404.
func fileErrStatus(err error) int {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}
