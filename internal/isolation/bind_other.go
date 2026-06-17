//go:build !linux

package isolation

import "fmt"

// bindWorkspaceIntoContainer is unsupported off Linux (no open_tree/move_mount/setns).
func bindWorkspaceIntoContainer(pid int, mount string) error {
	return fmt.Errorf("workspace live-mount unsupported on this platform")
}
