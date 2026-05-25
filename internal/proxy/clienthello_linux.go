//go:build linux

package proxy

import (
	"bufio"
	"encoding/binary"
	"fmt"
)

// peekTLSRecord returns the bytes of the next TLS record (header + body)
// without consuming them. We can't blindly Peek(1024) because a small
// ClientHello (e.g. curl 7.88 with a tight cipher list) may be only ~500
// bytes, and bufio.Reader.Peek blocks waiting for the missing bytes that
// never arrive. Reading the 5-byte record header first gives us the exact
// length to ask for.
func peekTLSRecord(br *bufio.Reader) ([]byte, error) {
	hdr, err := br.Peek(5)
	if err != nil || len(hdr) < 5 {
		return nil, fmt.Errorf("short TLS record header: %w", err)
	}
	bodyLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	const maxRecord = 16384 + 5 // RFC 8446 §5.1 plaintext fragment cap + header
	total := 5 + bodyLen
	if total > maxRecord {
		return nil, fmt.Errorf("TLS record too large: %d", total)
	}
	full, err := br.Peek(total)
	if err != nil && len(full) < total {
		return nil, err
	}
	return full, nil
}

// parseSNI extracts the server_name extension from a TLS ClientHello.
// Returns "" if the buffer is too short, malformed, or has no SNI.
//
// We only care about the host string for allowlist matching; we don't
// validate other ClientHello fields, and we tolerate truncation since
// the caller passes a Peek of the first ~1 KiB.
func parseSNI(b []byte) string {
	const recHdr = 5
	const hsHdr = 4
	if len(b) < recHdr+hsHdr || b[0] != 0x16 || b[recHdr] != 0x01 {
		return ""
	}
	p := recHdr + hsHdr + 2 + 32 // version(2) + random(32)
	if len(b) < p+1 {
		return ""
	}
	sidLen := int(b[p])
	p += 1 + sidLen
	if len(b) < p+2 {
		return ""
	}
	csLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2 + csLen
	if len(b) < p+1 {
		return ""
	}
	cmLen := int(b[p])
	p += 1 + cmLen
	if len(b) < p+2 {
		return ""
	}
	extLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	end := p + extLen
	if end > len(b) {
		end = len(b)
	}
	for p+4 <= end {
		extType := binary.BigEndian.Uint16(b[p:])
		extDataLen := int(binary.BigEndian.Uint16(b[p+2:]))
		p += 4
		if extType == 0x0000 { // server_name
			if extDataLen < 5 || p+5 > len(b) {
				return ""
			}
			// server_name_list_length(2) + name_type(1) + name_length(2)
			nameLen := int(binary.BigEndian.Uint16(b[p+3:]))
			if p+5+nameLen > len(b) {
				return ""
			}
			return string(b[p+5 : p+5+nameLen])
		}
		p += extDataLen
	}
	return ""
}
