//go:build !linux

package main

import "os"

// clearStaleMount off Linux has no lazy-unmount to perform (sandboxd only serves
// FUSE mounts on Linux), so it just removes the path. Present so the shared
// teardown/recovery code compiles on the dev/host build (darwin).
func clearStaleMount(path string) error {
	return os.RemoveAll(path)
}
