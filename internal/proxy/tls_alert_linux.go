//go:build linux

package proxy

import "net"

// TLS Alert record fields (RFC 5246 §7.2). We hand-craft the bytes instead
// of standing up a tls.Conn because the client's handshake hasn't been
// processed — we have no cipher suite or version, and don't need them: an
// Alert is the one TLS record a peer parses without prior handshake state.
const (
	tlsAlertFatal        byte = 2
	tlsAlertAccessDenied byte = 49
)

// writeTLSAlert writes a fatal TLS alert before any handshake bytes flow,
// so a peer sees a concrete error ("tlsv1 alert access denied") instead of
// the bare connection close it would otherwise see as SSL_ERROR_SYSCALL.
func writeTLSAlert(c net.Conn, level, description byte) {
	_, _ = c.Write([]byte{
		0x15,       // ContentType: alert
		0x03, 0x03, // ProtocolVersion: TLS 1.2 (max compatibility)
		0x00, 0x02, // record length
		level, description,
	})
}
