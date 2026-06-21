//go:build !linux

package main

import (
	"context"
	"net"
)

func dialGuestTCP(ctx context.Context, addr string, _ int) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}
