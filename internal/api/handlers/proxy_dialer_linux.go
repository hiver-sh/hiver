//go:build linux

package handlers

import (
	"context"
	"net"
	"syscall"
)

// markedDialContext returns a DialContext that stamps each new TCP socket with
// the given SO_MARK so the iptables OUTPUT/REDIRECT rule (which exempts marked
// sockets via -m mark --mark) lets the reverse proxy reach the user's service
// directly rather than being intercepted by sbxproxy.
func markedDialContext(mark int) func(context.Context, string, string) (net.Conn, error) {
	if mark == 0 {
		return nil
	}
	d := &net.Dialer{
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, mark)
			})
		},
	}
	return d.DialContext
}
