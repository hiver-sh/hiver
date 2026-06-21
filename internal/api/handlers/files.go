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

	"github.com/gin-gonic/gin"
	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/spec"
)

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

// UploadFile writes a multipart-uploaded file under one of the configured
// FUSE mounts. The `destination` form field must match a configured
// `fs[].mount` exactly; the file lands at `<destination>/<basename>`.
//
// The write goes through the backend's FileBridge, which bypasses the FUSE
// layer's per-mount ACLs — the API is a higher-privilege control surface than
// the workload, so operators seeding inputs shouldn't have to grant the agent
// rw on the same path.
func (h *Sandbox) UploadFile(c *gin.Context) {
	destination := c.PostForm("destination")
	if destination == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "missing form field: destination"})
		return
	}
	cleaned := filepath.Clean(destination)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "destination must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	header, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gen.Error{Error: err.Error()})
		return
	}
	name := filepath.Base(header.Filename)
	if name == "." || name == "/" || name == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "invalid file filename"})
		return
	}

	src, err := header.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	defer src.Close()

	n, err := h.iso.Files().Save(cleaned, name, h.mountRoutes(cfg), src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": filepath.Join(cleaned, name), "bytes": n})
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

	entries, err := h.iso.Files().List(cleaned, h.mountRoutes(cfg))
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
func (h *Sandbox) GetFile(c *gin.Context, params gen.GetFileParams) {
	if params.Path == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "missing query parameter: path"})
		return
	}
	cleaned := filepath.Clean(params.Path)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	rc, size, err := h.iso.Files().Open(cleaned, h.mountRoutes(cfg))
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
func (h *Sandbox) DeleteFile(c *gin.Context, params gen.DeleteFileParams) {
	if params.Path == "" {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "missing query parameter: path"})
		return
	}
	cleaned := filepath.Clean(params.Path)
	if !strings.HasPrefix(cleaned, "/") {
		c.JSON(http.StatusBadRequest, gen.Error{Error: "path must be absolute"})
		return
	}

	cfg, err := h.store.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}

	if err := h.iso.Files().Delete(cleaned, h.mountRoutes(cfg)); err != nil {
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
