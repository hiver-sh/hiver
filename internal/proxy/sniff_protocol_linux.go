//go:build linux

package proxy

import "bytes"

type protocol int

const (
	protoUnknown protocol = iota
	protoHTTP
	protoTLS
)

// sniffProtocol detects HTTP request lines and TLS ClientHello records from
// a few peeked bytes.
func sniffProtocol(peek []byte) protocol {
	// TLS record: type=0x16 (handshake), version 0x03xx, ... handshake type
	// at offset 5 = 0x01 (ClientHello).
	if len(peek) >= 6 && peek[0] == 0x16 && peek[1] == 0x03 && peek[5] == 0x01 {
		return protoTLS
	}
	for _, m := range [][]byte{
		[]byte("GET "), []byte("POST"), []byte("HEAD"),
		[]byte("PUT "), []byte("DELE"), []byte("OPTI"),
		[]byte("PATC"), []byte("TRAC"),
	} {
		if bytes.HasPrefix(peek, m) {
			return protoHTTP
		}
	}
	return protoUnknown
}
