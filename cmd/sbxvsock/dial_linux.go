//go:build linux

package main

import (
	"context"
	"net"
	"syscall"
)

// dialGuestTCP dials the guest exec port over the netns network, stamping the
// socket with SO_MARK so the pod's REDIRECT rule exempts it (the same bypass the
// ingress proxy uses).
func dialGuestTCP(ctx context.Context, addr string, mark int) (net.Conn, error) {
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, mark)
		})
	}}
	return d.DialContext(ctx, "tcp", addr)
}
