package isolation

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/blasten/hive/internal/spec"
)

// Files serves the management file API host-side. With host-side FUSE the
// workspace data lives in the host backend dirs, so paths under a configured
// mount resolve there directly (bypassing ACLs, like the container backend).
// Paths outside a mount live in the guest's overlay (its own block device)
// and are not host-visible while the VM runs, so they're reported as
// unsupported rather than silently reading the host root.
func (m *microvm) Files() FileBridge { return microvmHostFiles{} }

type microvmHostFiles struct{}

// backendPath maps an agent path under a configured mount to its host backend
// dir; ok is false when the path is outside every mount (guest-only overlay).
func (microvmHostFiles) backendPath(agentPath string, mounts []string) (string, bool) {
	cleaned := filepath.Clean(agentPath)
	var matched string
	for _, mnt := range mounts {
		if cleaned == mnt || strings.HasPrefix(cleaned, strings.TrimRight(mnt, "/")+"/") {
			if len(mnt) > len(matched) {
				matched = mnt
			}
		}
	}
	if matched == "" {
		return "", false
	}
	rel := strings.TrimPrefix(cleaned, matched)
	return filepath.Join(matched+spec.BackendSuffix, rel), true
}

var errGuestOnly = fmt.Errorf("path is in the guest overlay; only workspace mounts are accessible host-side under microvm")

func (f microvmHostFiles) List(agentPath string, mounts []string) ([]FileEntry, error) {
	host, ok := f.backendPath(agentPath, mounts)
	if !ok {
		return nil, errGuestOnly
	}
	es, err := os.ReadDir(host)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(es))
	for _, e := range es {
		info, err := e.Info()
		if err != nil {
			continue
		}
		var size int64
		if !e.IsDir() {
			size = info.Size()
		}
		out = append(out, FileEntry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}
	return out, nil
}

func (f microvmHostFiles) Open(agentPath string, mounts []string) (io.ReadCloser, int64, error) {
	host, ok := f.backendPath(agentPath, mounts)
	if !ok {
		return nil, 0, errGuestOnly
	}
	info, err := os.Stat(host)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("not a regular file")
	}
	fh, err := os.Open(host)
	if err != nil {
		return nil, 0, err
	}
	return fh, info.Size(), nil
}

func (f microvmHostFiles) Stat(agentPath string, mounts []string) (FileEntry, error) {
	host, ok := f.backendPath(agentPath, mounts)
	if !ok {
		return FileEntry{}, errGuestOnly
	}
	info, err := os.Stat(host)
	if err != nil {
		return FileEntry{}, err
	}
	var size int64
	if !info.IsDir() {
		size = info.Size()
	}
	return FileEntry{Name: filepath.Base(host), IsDir: info.IsDir(), Size: size}, nil
}

func (f microvmHostFiles) Save(agentDir, name string, mounts []string, r io.Reader) (int64, error) {
	host, ok := f.backendPath(agentDir, mounts)
	if !ok {
		return 0, errGuestOnly
	}
	if err := os.MkdirAll(host, 0o755); err != nil {
		return 0, err
	}
	target := filepath.Join(host, name)
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return 0, closeErr
	}
	return n, nil
}
