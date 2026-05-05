//go:build linux

package remotefs

import (
	"net"
	"net/http"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// markedHTTPClient returns an *http.Client whose dialer stamps SO_MARK
// on every outbound socket. sandboxd installs an iptables OUTPUT-chain
// nat REDIRECT in the sandbox-pod's netns to send all TCP through
// sbxproxy; sbxproxy escapes that rule by setting the same mark on
// its upstream sockets. sbxfuse needs the same escape when its remote
// store talks to a real cloud API — Drive, S3, GCS, OneDrive — so the
// platform's outbound traffic doesn't get mediated by the workload's
// proxy. Returns http.DefaultClient when mark is zero (local-only
// path, no escape needed).
//
// SO_MARK requires CAP_NET_ADMIN. The sandbox-pod runs with that cap,
// the agent's runc bundle is built without it, so this is the platform
// opting in — not a primitive the workload could replicate.
func markedHTTPClient(mark int) *http.Client {
	if mark == 0 {
		return http.DefaultClient
	}
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
			}); err != nil {
				return err
			}
			return sockErr
		},
	}
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}
