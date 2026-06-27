//go:build linux

package isolation

import (
	"os"

	"golang.org/x/sys/unix"
)

// reflinkFile makes dst a copy-on-write clone of src via the FICLONE ioctl: the
// two files share extents until one is written, so the clone is instant and
// independent (a later write to either leaves the other untouched). Supported on
// CoW filesystems (xfs, btrfs); returns an error on others (e.g. ext4), where
// the caller falls back to a full copy.
func reflinkFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
