//go:build linux

package proxy

import (
	"io"
	"net"
)

// peekedConn wraps a net.Conn so reads come from a bufio.Reader that still
// holds the peeked ClientHello bytes. tls.Server reads the handshake from
// this composite reader; writes go straight to the underlying conn.
type peekedConn struct {
	net.Conn
	r io.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }
