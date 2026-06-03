package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// DialGuest opens a connection to a guest vsock port through Firecracker's
// host-side vsock multiplexing socket (the uds_path from PUT /vsock).
//
// Firecracker's protocol for host-initiated connections: connect to the
// UDS, send the ASCII line "CONNECT <port>\n", and read back "OK
// <host_port>\n" once the guest has accepted. The returned net.Conn is a
// raw bidirectional stream to the guest listener on that port.
//
// See the Firecracker vsock docs ("Host-Initiated Connections").
func DialGuest(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock uds %s: %w", udsPath, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT %d: %w", port, err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT %d: read ack: %w", port, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT %d: unexpected ack %q", port, strings.TrimSpace(line))
	}
	// Clear the deadline set for the handshake; the caller manages I/O
	// timeouts on the live stream itself.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// WaitGuestPort blocks until a vsock connection to port succeeds (the guest
// agent is up and listening) or ctx is cancelled. The probe connection is
// closed immediately.
func WaitGuestPort(ctx context.Context, udsPath string, port uint32) error {
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		conn, err := DialGuest(probeCtx, udsPath, port)
		cancel()
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
