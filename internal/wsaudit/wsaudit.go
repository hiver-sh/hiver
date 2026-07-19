// Package wsaudit carries the WebSocket framing helpers shared by the egress
// proxy and the ingress reverse proxy so both can tunnel a ws:// connection
// while recording one audit chunk per application message. The frame parser,
// message assembler, and the upgrade/handshake helpers all live here; each
// caller supplies its own emit callback to route chunks into its own audit
// stream.
package wsaudit

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// WebSocket opcodes (RFC 6455 §5.2).
const (
	OpContinuation byte = 0x0
	OpText         byte = 0x1
	OpBinary       byte = 0x2
	OpClose        byte = 0x8
	OpPing         byte = 0x9
	OpPong         byte = 0xA
)

// AuditLimit caps both raw-frame buffering and message assembly for audit
// logging. Frames or messages exceeding this are recorded as size-only
// markers.
const AuditLimit = 64 * 1024

// Frame directions reported as the audit chunk's label so consumers can
// distinguish caller→upstream from upstream→caller.
const (
	DirUp   = "up"   // client → upstream
	DirDown = "down" // upstream → client
)

// forwardFrame copies exactly one WebSocket frame from src to dst.
// Returns the opcode, RSV1, FIN, the unmasked payload (nil when the
// frame exceeds AuditLimit or a read/write error occurs mid-frame),
// and the raw payload length. err is non-nil only when the frame
// could not be transferred in full.
func forwardFrame(src io.Reader, dst io.Writer) (opcode byte, rsv1, fin bool, payload []byte, payloadLen uint64, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(src, hdr[:]); err != nil {
		return 0, false, false, nil, 0, err
	}
	fin = hdr[0]&0x80 != 0
	rsv1 = hdr[0]&0x40 != 0
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	pl := uint64(hdr[1] & 0x7F)

	if _, err = dst.Write(hdr[:]); err != nil {
		return opcode, rsv1, fin, nil, 0, err
	}

	switch pl {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(src, ext[:]); err != nil {
			return opcode, rsv1, fin, nil, 0, err
		}
		if _, err = dst.Write(ext[:]); err != nil {
			return opcode, rsv1, fin, nil, 0, err
		}
		pl = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(src, ext[:]); err != nil {
			return opcode, rsv1, fin, nil, 0, err
		}
		if _, err = dst.Write(ext[:]); err != nil {
			return opcode, rsv1, fin, nil, 0, err
		}
		pl = binary.BigEndian.Uint64(ext[:])
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(src, maskKey[:]); err != nil {
			return opcode, rsv1, fin, nil, pl, err
		}
		if _, err = dst.Write(maskKey[:]); err != nil {
			return opcode, rsv1, fin, nil, pl, err
		}
	}

	if pl > AuditLimit {
		_, err = io.CopyN(dst, src, int64(pl))
		return opcode, rsv1, fin, nil, pl, err
	}

	data := make([]byte, pl)
	if _, err = io.ReadFull(src, data); err != nil {
		_, _ = dst.Write(data)
		return opcode, rsv1, fin, nil, pl, err
	}
	if _, err = dst.Write(data); err != nil {
		return opcode, rsv1, fin, nil, pl, err
	}
	if masked {
		for i := range data {
			data[i] ^= maskKey[i%4]
		}
	}
	return opcode, rsv1, fin, data, pl, nil
}

// StripExtensions removes the Sec-WebSocket-Extensions header from h. With no
// extensions offered, the server cannot negotiate permessage-deflate (or any
// other RSV-using extension), so every frame on the wire is plain bytes —
// exactly the application payload the audit log wants to record.
func StripExtensions(h http.Header) {
	h.Del("Sec-Websocket-Extensions")
}

// StripExtensionsRaw removes the Sec-WebSocket-Extensions header line from raw
// HTTP/1.1 upgrade bytes (case-insensitive on name). Used by the TLS-intercept
// path which writes the original request bytes verbatim. All other bytes are
// preserved exactly so the upstream's request fingerprint stays as close to the
// agent's as we can keep it.
func StripExtensionsRaw(raw []byte) []byte {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		return raw
	}
	head, rest := raw[:end], raw[end:]
	lines := bytes.Split(head, []byte("\r\n"))
	kept := lines[:0]
	for _, line := range lines {
		colon := bytes.IndexByte(line, ':')
		if colon >= 0 && strings.EqualFold(
			strings.TrimSpace(string(line[:colon])),
			"Sec-WebSocket-Extensions",
		) {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == len(lines) {
		return raw
	}
	return append(bytes.Join(kept, []byte("\r\n")), rest...)
}

// assembler reassembles a possibly-fragmented WebSocket message so the audit
// log records one chunk per application message rather than one per network
// frame. Assembly is bounded by AuditLimit; messages exceeding it surface as a
// size-only marker.
type assembler struct {
	buf      bytes.Buffer
	opcode   byte
	inflight bool
	overflow bool
}

// frame ingests one data frame (text, binary, or continuation) and returns the
// audit body — or "" while an in-flight message has not yet seen its FIN frame.
func (a *assembler) frame(opcode byte, fin bool, payload []byte, payloadLen uint64) string {
	switch opcode {
	case OpText, OpBinary:
		if payload == nil {
			return fmt.Sprintf("[%s %d bytes]", OpName(opcode), payloadLen)
		}
		if fin {
			return render(opcode, payload)
		}
		a.opcode = opcode
		a.inflight = true
		a.overflow = false
		a.buf.Reset()
		a.append(payload)
		return ""

	case OpContinuation:
		if !a.inflight {
			// Stray continuation; protocol error but record the size.
			return fmt.Sprintf("[continuation %d bytes]", payloadLen)
		}
		a.append(payload)
		if !fin {
			return ""
		}
		op, overflow := a.opcode, a.overflow
		rawLen := uint64(a.buf.Len())
		body := a.buf.Bytes()
		a.inflight = false
		if overflow {
			return fmt.Sprintf("[%s %d bytes (assembly overflow)]", OpName(op), rawLen)
		}
		return render(op, body)
	}
	return ""
}

func (a *assembler) append(payload []byte) {
	if a.overflow || payload == nil {
		return
	}
	if a.buf.Len()+len(payload) > AuditLimit {
		a.overflow = true
		return
	}
	a.buf.Write(payload)
}

func render(opcode byte, payload []byte) string {
	if opcode == OpText {
		return string(payload)
	}
	return fmt.Sprintf("[binary %d bytes]", len(payload))
}

func renderClose(payload []byte) string {
	if len(payload) < 2 {
		return "[close]"
	}
	code := binary.BigEndian.Uint16(payload[:2])
	if len(payload) > 2 {
		return fmt.Sprintf("[close %d %s]", code, payload[2:])
	}
	return fmt.Sprintf("[close %d]", code)
}

// OpName renders a WebSocket opcode as a short human label.
func OpName(op byte) string {
	switch op {
	case OpText:
		return "text"
	case OpBinary:
		return "binary"
	case OpContinuation:
		return "continuation"
	default:
		return fmt.Sprintf("op%d", op)
	}
}

// Forward pumps WebSocket frames from src to dst, calling emit once per
// fully-assembled application message. The dir (DirUp or DirDown) is passed to
// emit as the chunk label rather than inlined in the body, keeping payload
// bytes pristine. Callers strip Sec-WebSocket-Extensions from every upgrade, so
// frames on the wire are uncompressed and the recorded payload is exactly the
// application data. A frame with RSV1=1 in this regime indicates the server
// violated the stripped negotiation; we log and keep forwarding. host is used
// only for log context.
func Forward(src io.Reader, dst io.Writer, dir, host string, emit func(body, label string)) {
	var asm assembler
	for {
		opcode, rsv1, fin, payload, payloadLen, err := forwardFrame(src, dst)
		if err != nil {
			log.Printf("ws frame error: host=%s dir=%s op=%d err=%v", host, dir, opcode, err)
			return
		}
		if rsv1 {
			log.Printf("ws protocol violation: host=%s dir=%s op=%d rsv1=1 with stripped extensions", host, dir, opcode)
		}

		var body string
		switch opcode {
		case OpText, OpBinary, OpContinuation:
			body = asm.frame(opcode, fin, payload, payloadLen)
		case OpClose:
			body = renderClose(payload)
		case OpPing:
			body = "[ping]"
		case OpPong:
			body = "[pong]"
		}

		log.Printf("ws frame: host=%s dir=%s op=%d rsv1=%v fin=%v payloadLen=%d body=%q", host, dir, opcode, rsv1, fin, payloadLen, body)
		if body != "" {
			emit(body, dir)
		}
	}
}

// IsUpgrade reports whether r carries a WebSocket upgrade.
func IsUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// WriteUpgradeRequest writes a WebSocket upgrade request to w using the headers
// from req.Header directly, without the modifications that http.Request.Write
// makes:
//
//   - req.Write adds "User-Agent: Go-http-client/1.1" when the original
//     request carries no User-Agent. That tag identifies the proxy to the
//     upstream's WAF and can trigger security rejections.
//   - req.Write sorts headers in map-iteration order, which may differ from
//     the client's original ordering.
//
// Header names are still Go-canonical (e.g. "Sec-Websocket-Key") because
// http.ReadRequest normalises them on ingress; that canonicalisation is
// unavoidable. HTTP/1.1 requires case-insensitive header processing, so
// compliant upstreams accept it.
func WriteUpgradeRequest(w io.Writer, req *http.Request) error {
	bw := bufio.NewWriter(w)
	path := req.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	log.Printf("ws upgrade: sending %s %s HTTP/1.1 host=%s headers=%v", req.Method, path, req.Host, req.Header)
	if _, err := fmt.Fprintf(bw, "%s %s HTTP/1.1\r\nHost: %s\r\n", req.Method, path, req.Host); err != nil {
		return err
	}
	if err := req.Header.Write(bw); err != nil {
		return err
	}
	if _, err := io.WriteString(bw, "\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// WriteResponseHeaders writes the HTTP status line and headers of resp to w,
// followed by the blank line that separates headers from body. Used when the
// caller needs to stream the body separately (e.g. SSE or WebSocket).
func WriteResponseHeaders(w io.Writer, resp *http.Response) error {
	statusText := http.StatusText(resp.StatusCode)
	if statusText == "" {
		statusText = "Unknown"
	}
	if _, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText); err != nil {
		return err
	}
	// Forward upstream headers verbatim except Transfer-Encoding: the body
	// we're about to stream is already decoded by Go's http.ReadResponse.
	hdrs := resp.Header.Clone()
	hdrs.Del("Transfer-Encoding")
	if err := hdrs.Write(w); err != nil {
		return err
	}
	_, err := fmt.Fprint(w, "\r\n")
	return err
}
