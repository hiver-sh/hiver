//go:build linux

package runc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// MountOverlay sets up the overlayfs stack for the agent container:
//
//	lower  = RootfsDir  (base image, read-only; may be on Docker overlay2)
//	upper  = UpperDir   (all sandbox writes; on a private tmpfs)
//	work   = WorkDir    (overlayfs scratch; on the same tmpfs as upper)
//	merged = MergedDir  (unified view; becomes runc's root.path)
//
// upper and work live on a freshly-mounted tmpfs (ScratchDir) so they are
// never on an overlay filesystem. The kernel returns EINVAL when upper/work
// are on overlayfs (Docker's overlay2 backs /mnt inside the sandbox-pod),
// but the lower layer may be on overlayfs without restriction.
//
// FUSE bind-mounts added by runc shadow their paths inside the container's
// mount namespace, so writes to FUSE-managed paths go to the FUSE daemon
// and never reach UpperDir.
func MountOverlay() error {
	for _, dir := range []string{ScratchDir, MergedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	if err := syscall.Mount("tmpfs", ScratchDir, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount tmpfs scratch: %w", err)
	}
	for _, dir := range []string{UpperDir, WorkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			_ = syscall.Unmount(ScratchDir, 0)
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", RootfsDir, UpperDir, WorkDir)
	if err := syscall.Mount("overlay", MergedDir, "overlay", 0, opts); err != nil {
		_ = syscall.Unmount(ScratchDir, 0)
		return err
	}
	return nil
}

// UnmountOverlay tears down the overlay stack in reverse mount order.
// Must be called after the runc container exits.
func UnmountOverlay() error {
	var errs []error
	if err := syscall.Unmount(MergedDir, 0); err != nil {
		errs = append(errs, fmt.Errorf("unmount merged: %w", err))
	}
	if err := syscall.Unmount(ScratchDir, 0); err != nil {
		errs = append(errs, fmt.Errorf("unmount scratch tmpfs: %w", err))
	}
	return errors.Join(errs...)
}
