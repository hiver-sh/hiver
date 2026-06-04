//go:build !linux

package handlers

import (
	"context"
	"net"
)

func markedDialContext(_ int) func(context.Context, string, string) (net.Conn, error) {
	return nil
}
