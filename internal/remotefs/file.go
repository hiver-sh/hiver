package remotefs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FileStore is a [Store] backed by a local directory.
//
// Used as a stand-in for cloud backends: the Drive
// client, S3 SDK, GCS client, and Graph API client all sit behind the
// same Store interface, so swapping them in is a one-file change. The
// rest of the system (journal, bootstrap, FUSE wiring) doesn't care
// which store it's talking to.
type FileStore struct {
	root string
}

// NewFileStore returns a Store rooted at dir, creating it if missing.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileStore{root: dir}, nil
}

func (s *FileStore) hostPath(p string) string {
	clean := filepath.Clean("/" + strings.TrimPrefix(p, "/"))
	return filepath.Join(s.root, clean)
}

func (s *FileStore) List(_ context.Context, prefix string) ([]string, error) {
	base := s.hostPath(prefix)
	var out []string
	err := filepath.Walk(base, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		out = append(out, "/"+filepath.ToSlash(rel))
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

func (s *FileStore) Stat(_ context.Context, p string) (FileInfo, error) {
	st, err := os.Stat(s.hostPath(p))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileInfo{}, ErrNotExist
		}
		return FileInfo{}, err
	}
	return FileInfo{
		Path:  "/" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+strings.TrimPrefix(p, "/"))), "/"),
		Size:  st.Size(),
		Mtime: st.ModTime(),
		IsDir: st.IsDir(),
	}, nil
}

func (s *FileStore) ListDir(_ context.Context, dir string) ([]FileInfo, error) {
	base := s.hostPath(dir)
	dirCanon := filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(dir, "/")))
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{
			Path:  filepath.ToSlash(filepath.Join(dirCanon, e.Name())),
			Size:  st.Size(),
			Mtime: st.ModTime(),
			IsDir: e.IsDir(),
		})
	}
	return out, nil
}

func (s *FileStore) Get(_ context.Context, path string) (io.ReadCloser, error) {
	f, err := os.Open(s.hostPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotExist
	}
	return f, err
}

func (s *FileStore) Put(_ context.Context, path string, content io.Reader) error {
	p := s.hostPath(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, content)
	return err
}

func (s *FileStore) Delete(_ context.Context, path string) error {
	if err := os.Remove(s.hostPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *FileStore) Move(_ context.Context, src, dst string) error {
	srcP := s.hostPath(src)
	dstP := s.hostPath(dst)
	if err := os.MkdirAll(filepath.Dir(dstP), 0o755); err != nil {
		return err
	}
	return os.Rename(srcP, dstP)
}
