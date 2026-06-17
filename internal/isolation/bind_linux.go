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
func bindWorkspaceIntoContainer(pid int, mount string) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // intentionally never unlocked; see above
		errc <- moveMountIntoNS(pid, mount)
	}()
	return <-errc
}

func moveMountIntoNS(pid int, mount string) error {
	pidStr := strconv.Itoa(pid)

	// Clone the source mount (the host sbxfuse mount) into a detached fd while
	// still in sandboxd's mount namespace, where it is reachable by path.
	srcFd, err := unix.OpenTree(unix.AT_FDCWD, mount, unix.OPEN_TREE_CLONE|unix.AT_RECURSIVE)
	if err != nil {
		return fmt.Errorf("open_tree %s: %w", mount, err)
	}
	defer unix.Close(srcFd)

	// Create the mountpoint in the container's filesystem (its overlay is
	// host-reachable through /proc/<pid>/root) and open the container root as a
	// dirfd. The destination is then resolved relative to this dirfd *after*
	// setns, so it lands on the container's own mount instance rather than the
	// host's view of it (which is why a pre-setns target fd produced a bare upper
	// dir with no fuse on it).
	containerRoot := filepath.Join("/proc", pidStr, "root")
	if err := os.MkdirAll(filepath.Join(containerRoot, mount), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mount, err)
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
	rel := strings.TrimPrefix(mount, "/")
	if err := unix.MoveMount(srcFd, "", rootFd, rel, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount → %s: %w", mount, err)
	}
	return nil
}
