//go:build linux

// Package netmark provides a net.Dialer.Control hook that stamps
// SO_MARK on dialed sockets. The mark is invisible to the wire but
// lets iptables `-m mark --mark <mark>` match these packets and
// exempt them from the sandbox-pod's OUTPUT REDIRECT — the same
// trick sbxproxy and sbxfuse use to escape transparent egress.
package netmark

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// Control returns a net.Dialer.Control func that sets SO_MARK on
// every socket the dialer opens. Requires CAP_NET_ADMIN.
func Control(mark int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
		}); err != nil {
			return err
		}
		return sockErr
	}
}
