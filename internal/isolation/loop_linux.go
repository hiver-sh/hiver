//go:build linux

package isolation

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// loopAllocMu serializes loop-device allocation within this process so two concurrent
// creates can't be handed the same LOOP_CTL_GET_FREE index and collide on
// LOOP_CONFIGURE. (Cross-process/-pod races on the node-global loop pool are still
// possible and handled by the EBUSY retry below.)
var loopAllocMu sync.Mutex

// loopAttach binds backingFile to a free /dev/loopN and returns its path. When
// readonly it opens the backing file O_RDONLY and sets LO_FLAGS_READ_ONLY — used for
// the shared base-overlay origin, which is never written. The kernel takes its own
// reference to the backing file, so the fds opened here are closed before returning;
// loopDetach releases the association later. The /dev/loopN nodes must already exist
// (ensureLoopNodes mknods them in the sandbox's tmpfs /dev).
func loopAttach(backingFile string, readonly bool) (string, error) {
	openFlags := os.O_RDWR
	if readonly {
		openFlags = os.O_RDONLY
	}
	back, err := os.OpenFile(backingFile, openFlags, 0)
	if err != nil {
		return "", fmt.Errorf("open backing %s: %w", backingFile, err)
	}
	defer back.Close()
	cfg := unix.LoopConfig{Fd: uint32(back.Fd())}
	if readonly {
		cfg.Info.Flags = unix.LO_FLAGS_READ_ONLY
	}

	loopAllocMu.Lock()
	defer loopAllocMu.Unlock()
	ctrl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open loop-control: %w", err)
	}
	defer ctrl.Close()

	// LOOP_CTL_GET_FREE then LOOP_CONFIGURE is racy: a concurrent allocator (another
	// pod on this node — loop devices are node-global) can grab the same free index
	// first, so our LOOP_CONFIGURE gets EBUSY. Retry with a fresh free device, as
	// losetup does. The in-process mutex above removes our own contention, so this
	// only spins on genuine cross-process races.
	const maxAttempts = 128
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		idx, err := unix.IoctlRetInt(int(ctrl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return "", fmt.Errorf("LOOP_CTL_GET_FREE: %w", err)
		}
		loopPath := fmt.Sprintf("/dev/loop%d", idx)
		loop, err := os.OpenFile(loopPath, os.O_RDWR, 0)
		if err != nil {
			lastErr = fmt.Errorf("open %s: %w", loopPath, err)
			continue
		}
		err = unix.IoctlLoopConfigure(int(loop.Fd()), &cfg)
		loop.Close()
		if err == nil {
			return loopPath, nil
		}
		if errors.Is(err, unix.EBUSY) {
			lastErr = err
			continue // raced for this device; try another
		}
		return "", fmt.Errorf("LOOP_CONFIGURE %s: %w", loopPath, err)
	}
	return "", fmt.Errorf("loopAttach %s: no free loop device after %d attempts: %w", backingFile, maxAttempts, lastErr)
}

// loopMountExt4 attaches img to a loop device and mounts it read-write at
// mountpoint (ext4), returning the loop path to pass back to loopUnmount. Used by
// the snapshot-capture path; replaces a `mount -o loop` subprocess. Read-write
// because the captured overlay's journal is dirty (the guest powered off without a
// clean unmount) and the kernel refuses a read-only mount it can't replay.
func loopMountExt4(img, mountpoint string) (string, error) {
	loop, err := loopAttach(img, false)
	if err != nil {
		return "", err
	}
	if err := unix.Mount(loop, mountpoint, "ext4", 0, ""); err != nil {
		_ = loopDetach(loop)
		return "", fmt.Errorf("mount %s at %s: %w", loop, mountpoint, err)
	}
	return loop, nil
}

// loopUnmount undoes loopMountExt4: unmount the filesystem, then detach the loop.
// Best-effort (teardown path). Replaces an `umount` subprocess.
func loopUnmount(mountpoint, loopPath string) {
	_ = unix.Unmount(mountpoint, 0)
	if loopPath != "" {
		_ = loopDetach(loopPath)
	}
}

// loopDetach releases the backing file from a loop device (LOOP_CLR_FD).
func loopDetach(loopPath string) error {
	f, err := os.OpenFile(loopPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.IoctlSetInt(int(f.Fd()), unix.LOOP_CLR_FD, 0)
}

