//go:build !linux

package main

import "context"

// reapOrphans is a no-op off Linux: sandboxd only runs as a pod's PID 1 on Linux,
// and the /proc-based orphan sweep is Linux-specific. The dev/host build (darwin)
// runs sandboxd as an ordinary process, where the Go runtime + cmd.Wait() already
// reap every child it spawns.
func reapOrphans(context.Context) {}
