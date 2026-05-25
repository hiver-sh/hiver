//go:build linux

package proxy

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// soOriginalDst mirrors Linux's iptables nat-table flag.
// /usr/include/linux/netfilter_ipv4.h.
const soOriginalDst = 80

// getOriginalDst reads SO_ORIGINAL_DST off a redirected TCP connection.
// Returns "ip:port" suitable for net.Dial.
func getOriginalDst(c *net.TCPConn) (string, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return "", err
	}
	var addr string
	var sockErr error
	ctlErr := rc.Control(func(fd uintptr) {
		var raw [16]byte // sizeof(sockaddr_in)
		size := uint32(len(raw))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(syscall.SOL_IP),
			uintptr(soOriginalDst),
			uintptr(unsafe.Pointer(&raw[0])),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			sockErr = errno
			return
		}
		// struct sockaddr_in: family(2) | port(2 BE) | addr(4) | pad(8)
		port := binary.BigEndian.Uint16(raw[2:4])
		ip := net.IPv4(raw[4], raw[5], raw[6], raw[7])
		addr = fmt.Sprintf("%s:%d", ip.String(), port)
	})
	if ctlErr != nil {
		return "", ctlErr
	}
	if sockErr != nil {
		return "", sockErr
	}
	return addr, nil
}
