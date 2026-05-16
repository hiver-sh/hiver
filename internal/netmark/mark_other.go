//go:build !linux

package netmark

import (
	"errors"
	"syscall"
)

// Control is a stub on non-Linux. SO_MARK is Linux-specific; a dialer
// configured with this Control will refuse to open sockets, matching
// the fact that transparent egress (and the sandbox runtime itself)
// is Linux-only.
func Control(_ int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, _ syscall.RawConn) error {
		return errors.New("netmark: SO_MARK only supported on Linux")
	}
}
