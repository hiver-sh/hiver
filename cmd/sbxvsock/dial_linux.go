//go:build linux

package main

import (
	"context"
	"net"
	"syscall"
	"time"
)

// dialGuestTCP dials the guest exec port over the netns network, stamping the
// socket with SO_MARK so the pod's REDIRECT rule exempts it (the same bypass the
// ingress proxy uses).
//
// TCP keepalive is enabled so a vanished guest is detected even when no RST
// arrives: a DELETE kills the VMM (the host-side ctx cancel reaps this bridge),
// but a guest that crashes/OOMs without a delete leaves the read loop blocked on
// a half-open connection — nothing FINs it. Keepalive probes surface that as a
// read error within ~Idle+Interval*Count so this process exits instead of
// lingering.
func dialGuestTCP(ctx context.Context, addr string, mark int) (net.Conn, error) {
	d := net.Dialer{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     30 * time.Second,
			Interval: 10 * time.Second,
			Count:    3,
		},
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, mark)
			})
		},
	}
	return d.DialContext(ctx, "tcp", addr)
}
