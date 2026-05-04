//go:build linux

package proxy

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// soMarkControl returns a net.Dialer.Control func that stamps SO_MARK on
// every socket the proxy opens for upstream traffic. The mark is invisible
// outside the host but lets iptables `-m mark --mark <mark>` match
// proxy-originated packets and exempt them from the OUTPUT REDIRECT that
// transparent egress relies on.
//
// Requires CAP_NET_ADMIN. The sandbox-pod runs --privileged today, so this
// is satisfied; trimming caps is a follow-up.
func soMarkControl(mark int) func(network, address string, c syscall.RawConn) error {
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
