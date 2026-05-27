// Package snapshot captures and restores overlayfs upper-layer and local
// filesystem backend state.
//
// Capture walks the paths listed in include and writes them as a
// gzip-compressed tar to dst. Each include entry is resolved to either a
// local FS backend directory (for FUSE-mounted paths) or the overlayfs upper
// layer (for everything else) via the mounts table. Restore extracts a
// captured tar back into the correct host directories so the next container
// start sees the snapshotted state.
//
// Include entries may be absolute container paths (/home/user) or glob-style
// patterns (/home/user/*); a trailing /* is stripped to obtain the base
// directory, which is then walked recursively.
//
// Local FS mounts are automatically appended to include so callers don't
// have to repeat them.
package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MountSource maps a container path to its backing host directory.
// For local FS mounts, HostDir is the -backend directory that sbxfuse
// passthrough-mounts; content written by the agent lives there, not in
// the overlayfs upper layer.
type MountSource struct {
	ContainerPath string // absolute container path, e.g. /workspace
	HostDir       string // host directory containing the data, e.g. /workspace-backend
}

// Capture creates a gzip-compressed tar at dst.
//
// For each path in include the source is determined by the mounts table:
// if the path falls under a MountSource.ContainerPath the content is read
// from MountSource.HostDir; otherwise it is read from upperDir. Paths not
// present in the resolved host directory are silently skipped.
func Capture(dst, upperDir string, mounts []MountSource, include []string) error {
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	written := 0
	for _, pattern := range include {
		base := baseDir(pattern)
		hostRoot, tarPrefix := resolveSource(upperDir, mounts, base)
		if _, err := os.Lstat(hostRoot); os.IsNotExist(err) {
			log.Printf("snapshot: capture: %s not present, skipping", pattern)
			continue
		}
		if err := walkIntoTar(tw, hostRoot, tarPrefix); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return fmt.Errorf("walk %s: %w", hostRoot, err)
		}
		written++
		log.Printf("snapshot: capture: added %s (from %s)", base, hostRoot)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	log.Printf("snapshot: capture: wrote %s (%d path(s))", dst, written)
	return nil
}

// Restore extracts the gzip-compressed tar at src back into the correct host
// directories. Each tar entry's name encodes the absolute container path
// (without the leading /); the mounts table routes it to the appropriate
// backend directory or upperDir.
func Restore(src, upperDir string, mounts []MountSource) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		target := resolveTarget(upperDir, mounts, hdr.Name)
		if target == "" {
			log.Printf("snapshot: restore: skipping unsafe path %q", hdr.Name)
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, hdr.FileInfo().Mode()); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
			if err := os.Chmod(target, hdr.FileInfo().Mode()); err != nil {
				return fmt.Errorf("chmod %s: %w", target, err)
			}
			if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
				return fmt.Errorf("lchown %s: %w", target, err)
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", target, err)
			}
			if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
				return fmt.Errorf("lchown %s: %w", target, err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			fh, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(fh, tr); err != nil {
				fh.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			fh.Close()
			if err := os.Chmod(target, hdr.FileInfo().Mode()); err != nil {
				return fmt.Errorf("chmod %s: %w", target, err)
			}
			if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
				return fmt.Errorf("lchown %s: %w", target, err)
			}
		}
		count++
	}
	log.Printf("snapshot: restore: extracted %d entries from %s", count, src)
	return nil
}

// SnapshotPath returns the canonical file name for a snapshot key.
func SnapshotPath(dir, key string) string {
	return filepath.Join(dir, "snapshot-"+key+".tar.gz")
}

// resolveSource returns the host directory to read from and the tar entry
// prefix for a given absolute container path. If the path falls under a
// MountSource the backing HostDir is used; otherwise upperDir is used.
func resolveSource(upperDir string, mounts []MountSource, containerPath string) (hostRoot, tarPrefix string) {
	if m := bestMount(mounts, containerPath); m != nil {
		rel := strings.TrimPrefix(containerPath, m.ContainerPath)
		hostRoot = filepath.Join(m.HostDir, rel)
		tarPrefix = strings.TrimLeft(containerPath, "/")
		return
	}
	hostRoot = filepath.Join(upperDir, containerPath)
	tarPrefix = strings.TrimLeft(containerPath, "/")
	return
}

// resolveTarget maps a tar entry name back to a host path for restore.
// Returns "" if the path is unsafe (traversal attempt).
func resolveTarget(upperDir string, mounts []MountSource, tarName string) string {
	containerPath := "/" + filepath.Clean(tarName)
	if m := bestMount(mounts, containerPath); m != nil {
		rel := strings.TrimPrefix(containerPath, m.ContainerPath)
		target := filepath.Join(m.HostDir, rel)
		// Safety: must stay within HostDir.
		if !strings.HasPrefix(target, m.HostDir+string(filepath.Separator)) && target != m.HostDir {
			return ""
		}
		return target
	}
	target := filepath.Join(upperDir, containerPath)
	if !strings.HasPrefix(target, upperDir+string(filepath.Separator)) && target != upperDir {
		return ""
	}
	return target
}

// bestMount returns the longest-prefix matching MountSource for containerPath,
// or nil if none match.
func bestMount(mounts []MountSource, containerPath string) *MountSource {
	best := -1
	for i, m := range mounts {
		if containerPath != m.ContainerPath && !strings.HasPrefix(containerPath, m.ContainerPath+"/") {
			continue
		}
		if best < 0 || len(m.ContainerPath) > len(mounts[best].ContainerPath) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &mounts[best]
}

// baseDir strips trailing glob suffixes (e.g. /* or /**) to get the
// directory to walk. /home/user/* → /home/user.
func baseDir(pattern string) string {
	p := strings.TrimRight(pattern, "/")
	for strings.HasSuffix(p, "/*") || strings.HasSuffix(p, "/**") {
		p = filepath.Dir(p)
	}
	return p
}

// walkIntoTar walks hostRoot recursively and writes each entry into tw.
// tarPrefix is prepended to the relative path to form the tar entry name,
// encoding the container-absolute path without a leading slash.
func walkIntoTar(tw *tar.Writer, hostRoot, tarPrefix string) error {
	return filepath.Walk(hostRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostRoot, path)
		if err != nil {
			return err
		}
		name := filepath.Join(tarPrefix, rel)
		var hdr *tar.Header
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr = &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     name,
				Linkname: link,
				Mode:     int64(info.Mode()),
				ModTime:  info.ModTime(),
			}
			if sys, ok := info.Sys().(*syscall.Stat_t); ok {
				hdr.Uid = int(sys.Uid)
				hdr.Gid = int(sys.Gid)
			}
		} else {
			hdr, err = tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = name
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		fh, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		_, err = io.Copy(tw, fh)
		return err
	})
}
