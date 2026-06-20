//go:build !linux

package isolation

import "fmt"

// bindWorkspaceIntoContainer is unsupported off Linux (no open_tree/move_mount/setns).
func bindWorkspaceIntoContainer(pid int, hostMount, guestMount string) error {
	return fmt.Errorf("workspace live-mount unsupported on this platform")
}

// unmountWorkspaceFromContainer is unsupported off Linux (no setns/umount-in-ns).
func unmountWorkspaceFromContainer(pid int, guestMount string) error {
	return fmt.Errorf("workspace live-unmount unsupported on this platform")
}
