//go:build !linux

package proxy

import (
	"errors"
	"syscall"
)

// soMarkControl is a stub on non-Linux. SO_MARK is Linux-specific; if a
// Config asks for it on another platform the dialer will refuse to open
// upstream connections. Transparent mode itself is also Linux-only.
func soMarkControl(_ int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, _ syscall.RawConn) error {
		return errors.New("proxy: SO_MARK only supported on Linux")
	}
}
