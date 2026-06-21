//go:build !linux

package isolation

import (
	"context"
	"errors"
	"net"
)

func (m *microvm) setupPackedNetMicrovm(context.Context, int, int, int) error {
	return errors.New("packed microvm networking not supported on this platform")
}

func (m *microvm) teardownPackedNetMicrovm(context.Context) {}

func listenTCPInNetns(_, addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

func (m *microvm) dialGuest(ctx context.Context, port uint32) (net.Conn, error) {
	return nil, errors.New("guest dial not supported on this platform")
}
