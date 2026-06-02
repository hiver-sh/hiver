package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/gin-gonic/gin"
)

// UploadFile writes a multipart-uploaded file under one of the
// configured FUSE mounts. The `destination` form field must match a
// configured `fs[].mount` exactly; the file lands at
// `<destination>/<basename(filename)>` (the agent-visible path).
//
// The write bypasses the FUSE layer: we open the underlying backend
// directory directly so the per-mount ACLs that gate the agent do
// NOT apply. The API is a higher-privilege control surface than the
// workload — operators seeding inputs over /v1/file should not have
// to grant the agent rw on the same path.
func (h *SandboxHandlers) UploadFile(c *gin.Context) {
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

	hostDir := h.resolveHostPath(cfg, cleaned)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	hostTarget := filepath.Join(hostDir, name)
	agentTarget := filepath.Join(cleaned, name)

	src, err := header.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	defer src.Close()

	dst, err := os.OpenFile(hostTarget, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	n, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(hostTarget)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: copyErr.Error()})
		return
	}
	if closeErr != nil {
		_ = os.Remove(hostTarget)
		c.JSON(http.StatusInternalServerError, gen.Error{Error: closeErr.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"path": agentTarget, "bytes": n})
}

// ListDirectory returns the immediate children of a directory. For the
// root path ("/") it lists the overlayfs upper layer so callers see every
// path the container has written to. For any other path the read is served
// from the FUSE mount backend when the path is under a configured mount,
// falling back to the upper layer otherwise. Either way the read bypasses
// sbxfuse ACLs.
func (h *SandboxHandlers) ListDirectory(c *gin.Context, params gen.ListDirectoryParams) {
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

	target := h.resolveHostPath(cfg, cleaned)

	entries, err := os.ReadDir(target)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		c.JSON(status, gen.Error{Error: err.Error()})
		return
	}

	type dirEntry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	result := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		size := int64(0)
		if !e.IsDir() {
			size = info.Size()
		}
		result = append(result, dirEntry{
			Name:  e.Name(),
			Path:  filepath.Join(cleaned, e.Name()),
			IsDir: e.IsDir(),
			Size:  size,
		})
	}
	c.JSON(http.StatusOK, gin.H{"entries": result})
}

// GetFile streams a file from the sandbox filesystem. The path resolves
// via the same FUSE-backend-first / upper-layer-fallback logic as
// ListDirectory, and bypasses sbxfuse ACLs.
func (h *SandboxHandlers) GetFile(c *gin.Context, params gen.GetFileParams) {
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

	target := h.resolveHostPath(cfg, cleaned)

	info, err := os.Stat(target)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		c.JSON(status, gen.Error{Error: err.Error()})
		return
	}
	if !info.Mode().IsRegular() {
		c.JSON(http.StatusNotFound, gen.Error{Error: "not a regular file"})
		return
	}

	f, err := os.Open(target)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gen.Error{Error: err.Error()})
		return
	}
	defer f.Close()

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(cleaned)))
	c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
	c.DataFromReader(http.StatusOK, info.Size(), "application/octet-stream", f, nil)
}
