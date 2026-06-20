//go:build linux

package isolation

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// bindWorkspaceIntoContainer makes a host-side workspace mount visible inside the
// already-running agent container (pid) — the case of a workspace added by a
// runtime PUT /v1/config, which runc's launch-time bundle mounts can't cover.
//
// It clones the source mount with open_tree while still in sandboxd's namespace
// (where the path is reachable), enters the container's mount namespace, and
// re-attaches it with move_mount. The destination is resolved through
// /proc/<pid>/root — the container's pivoted root, not the host overlay instance
// — so the mount lands in the container's view.
//
// The work runs on a dedicated OS thread that is locked and never unlocked:
// setns into a mount namespace mutates the thread irreversibly, so the Go runtime
// retires the thread when the goroutine returns rather than handing a poisoned
// thread back to the pool. This keeps it in-process — no re-exec helper.
func bindWorkspaceIntoContainer(pid int, hostMount, guestMount string) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // intentionally never unlocked; see above
		errc <- moveMountIntoNS(pid, hostMount, guestMount)
	}()
	return <-errc
}

// unmountWorkspaceFromContainer detaches a workspace mount from the already-running
// agent container (pid) — the reverse of bindWorkspaceIntoContainer, for a workspace
// dropped by a runtime PUT /v1/config. The mount lives only in the container's mount
// namespace (move_mount'd there at add time), so the umount must run inside it.
//
// As with the add path, this runs on a dedicated, never-unlocked OS thread because
// setns into a mount namespace mutates the thread irreversibly.
func unmountWorkspaceFromContainer(pid int, guestMount string) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // intentionally never unlocked; see bindWorkspaceIntoContainer
		errc <- umountFromNS(pid, guestMount)
	}()
	return <-errc
}

func umountFromNS(pid int, guestMount string) error {
	pidStr := strconv.Itoa(pid)

	// Pin the container root (a dentry in the container's mount namespace, reachable
	// from the host through the magic /proc/<pid>/root link) before entering the
	// namespace. setns changes which mounts a path walk sees but not the thread's
	// root dir, so an absolute "/workspace" would still resolve against sandboxd's
	// identically-named tree; resolving relative to this fd lands in the container's.
	rootFd, err := unix.Open(filepath.Join("/proc", pidStr, "root"), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open container root: %w", err)
	}
	defer unix.Close(rootFd)

	nsFd, err := unix.Open(filepath.Join("/proc", pidStr, "ns", "mnt"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open mount ns: %w", err)
	}
	defer unix.Close(nsFd)
	// Go threads share filesystem attributes (CLONE_FS); setns(CLONE_NEWNS) refuses
	// until this locked, soon-discarded thread unshares them.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare fs: %w", err)
	}
	if err := unix.Setns(nsFd, unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns: %w", err)
	}
	// Point cwd at the pinned container root so the relative guest path resolves
	// inside the container's filesystem (O_PATH dirfds are valid for fchdir).
	if err := unix.Fchdir(rootFd); err != nil {
		return fmt.Errorf("fchdir container root: %w", err)
	}
	rel := strings.TrimPrefix(guestMount, "/")
	// MNT_DETACH (lazy): the agent may hold a cwd or open fd under the mount, which
	// would make a synchronous umount fail with EBUSY; detach it from the tree now
	// and let the kernel reap it once the last reference drops.
	if err := unix.Unmount(rel, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("umount %s: %w", guestMount, err)
	}
	return nil
}

func moveMountIntoNS(pid int, hostMount, guestMount string) error {
	pidStr := strconv.Itoa(pid)

	// Clone the source mount (the host sbxfuse mount) into a detached fd while
	// still in sandboxd's mount namespace, where it is reachable by path.
	srcFd, err := unix.OpenTree(unix.AT_FDCWD, hostMount, unix.OPEN_TREE_CLONE|unix.AT_RECURSIVE)
	if err != nil {
		return fmt.Errorf("open_tree %s: %w", hostMount, err)
	}
	defer unix.Close(srcFd)

	// Create the mountpoint in the container's filesystem (its overlay is
	// host-reachable through /proc/<pid>/root) and open the container root as a
	// dirfd. The destination is then resolved relative to this dirfd *after*
	// setns, so it lands on the container's own mount instance rather than the
	// host's view of it (which is why a pre-setns target fd produced a bare upper
	// dir with no fuse on it).
	containerRoot := filepath.Join("/proc", pidStr, "root")
	if err := os.MkdirAll(filepath.Join(containerRoot, guestMount), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", guestMount, err)
	}
	rootFd, err := unix.Open(containerRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open container root: %w", err)
	}
	defer unix.Close(rootFd)

	// Enter the container's mount namespace (this thread only) and attach the
	// clone at the destination, resolved relative to the container root.
	nsFd, err := unix.Open(filepath.Join("/proc", pidStr, "ns", "mnt"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open mount ns: %w", err)
	}
	defer unix.Close(nsFd)
	// Go creates its threads sharing filesystem attributes (CLONE_FS), and
	// setns(CLONE_NEWNS) refuses (EINVAL) on a thread that still shares them.
	// Unshare on this locked, soon-discarded thread first.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare fs: %w", err)
	}
	if err := unix.Setns(nsFd, unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns: %w", err)
	}
	rel := strings.TrimPrefix(guestMount, "/")
	if err := unix.MoveMount(srcFd, "", rootFd, rel, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount → %s: %w", guestMount, err)
	}
	return nil
}
