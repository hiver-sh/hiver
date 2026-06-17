//go:build linux

package remotefs

import (
	"context"
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
//
// The mark has to be stamped on two distinct sockets, not one. A
// net.Dialer.Control callback only fires for the final connection — by then
// the hostname is already resolved. The DNS lookup that precedes it runs on
// the resolver's own sockets, which would otherwise be unmarked and so get
// caught by sandboxd's `--dport 53 -m mark ! --mark` REDIRECT into sbxproxy's
// DNS sink (which answers every query with a bogus address). So the dialer is
// given a Resolver whose own dials are marked too; a marked DNS query skips the
// sink rule and falls through to the real resolver. Miss this and a remote
// store resolves every hostname to the sink and every connection times out.
func markedHTTPClient(mark int) *http.Client {
	if mark == 0 {
		return http.DefaultClient
	}
	control := func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
		}); err != nil {
			return err
		}
		return sockErr
	}
	// PreferGo forces the pure-Go resolver so the Dial hook (and thus the mark)
	// is honoured; the cgo resolver would bypass it and leak unmarked queries.
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 30 * time.Second, Control: control}
			return d.DialContext(ctx, network, address)
		},
	}
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   control,
		Resolver:  resolver,
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
