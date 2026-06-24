//go:build linux

package isolation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	devmapper "github.com/anatol/devmapper.go"
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

// loopFindByBacking returns the loop device paths whose backing file is backingFile,
// read from sysfs. Used to detach a loop left over a stale COW file by a crashed
// teardown before the path is reused. Best-effort: unreadable entries are skipped.
func loopFindByBacking(backingFile string) []string {
	var out []string
	matches, _ := filepath.Glob("/sys/block/loop*/loop/backing_file")
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		// sysfs reports the path with a trailing newline; a since-deleted backing file
		// gets a " (deleted)" suffix.
		bf := strings.TrimSuffix(strings.TrimSpace(string(b)), " (deleted)")
		if bf == backingFile {
			// /sys/block/loopN/loop/backing_file -> /dev/loopN
			name := filepath.Base(filepath.Dir(filepath.Dir(m)))
			out = append(out, "/dev/"+name)
		}
	}
	return out
}

// cowChunkBytes is the dm-snapshot chunk size. 4096 B = 8 sectors; the library
// converts to sectors (ChunkSize/512) when building the table.
const cowChunkBytes = 4096

// dmCreateSnapshot creates+loads+resumes a dm-snapshot device named name (origin +
// COW devices, length in 512-byte sectors), mknods its /dev/mapper node, and returns
// the node path. The metadata is transient (Persistent:false → "N"): exception state
// is kept in kernel RAM, not written to the COW store, since the store is ephemeral.
// On any failure the half-built device is removed.
func dmCreateSnapshot(name, originDev, cowDev string, sectors int64) (string, error) {
	table := devmapper.SnapshotTable{
		Start:        0,
		Length:       uint64(sectors),
		OriginDevice: originDev,
		COWDevice:    cowDev,
		Persistent:   false,
		ChunkSize:    cowChunkBytes,
	}
	if err := devmapper.CreateAndLoad(name, "", 0, table); err != nil {
		return "", fmt.Errorf("dm create %s: %w", name, err)
	}
	// The library only talks to /dev/mapper/control; with no udev in the sandbox's
	// tmpfs /dev the device node isn't created for us, so make it from the devno.
	info, err := devmapper.InfoByName(name)
	if err != nil {
		_ = dmRemove(name)
		return "", fmt.Errorf("dm info %s: %w", name, err)
	}
	if err := os.MkdirAll("/dev/mapper", 0o755); err != nil {
		_ = dmRemove(name)
		return "", fmt.Errorf("mkdir /dev/mapper: %w", err)
	}
	node := "/dev/mapper/" + name
	_ = os.Remove(node) // a stale node from a prior incarnation
	devNo := int(unix.Mkdev(unix.Major(info.DevNo), unix.Minor(info.DevNo)))
	if err := unix.Mknod(node, unix.S_IFBLK|0o600, devNo); err != nil {
		_ = dmRemove(name)
		return "", fmt.Errorf("mknod %s: %w", node, err)
	}
	return node, nil
}

// dmRemove removes the dm device by name and its /dev/mapper node. Best-effort.
func dmRemove(name string) error {
	err := devmapper.Remove(name)
	_ = os.Remove("/dev/mapper/" + name)
	return err
}
