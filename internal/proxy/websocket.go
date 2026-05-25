package proxy

import (
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
	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA
)

// wsAuditLimit caps both raw-frame buffering and message assembly for
// audit logging. Frames or messages exceeding this are recorded as
// size-only markers.
const wsAuditLimit = 64 * 1024

// wsForwardFrame copies exactly one WebSocket frame from src to dst.
// Returns the opcode, RSV1, FIN, the unmasked payload (nil when the
// frame exceeds wsAuditLimit or a read/write error occurs mid-frame),
// and the raw payload length. err is non-nil only when the frame
// could not be transferred in full.
func wsForwardFrame(src io.Reader, dst io.Writer) (opcode byte, rsv1, fin bool, payload []byte, payloadLen uint64, err error) {
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

	if pl > wsAuditLimit {
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

// stripWebSocketExtensions removes the Sec-WebSocket-Extensions header
// from h. With no extensions offered, the server cannot negotiate
// permessage-deflate (or any other RSV-using extension), so every
// frame on the wire is plain bytes — exactly the application payload
// the audit log wants to record.
func stripWebSocketExtensions(h http.Header) {
	h.Del("Sec-Websocket-Extensions")
}

// stripWebSocketExtensionsRaw removes the Sec-WebSocket-Extensions
// header line from raw HTTP/1.1 upgrade bytes (case-insensitive on
// name). Used by the TLS-intercept path which writes the original
// request bytes verbatim. All other bytes are preserved exactly so
// the upstream's request fingerprint stays as close to the agent's
// as we can keep it.
func stripWebSocketExtensionsRaw(raw []byte) []byte {
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

// wsAssembler reassembles a possibly-fragmented WebSocket message so
// the audit log records one chunk per application message rather than
// one per network frame. Assembly is bounded by wsAuditLimit; messages
// exceeding it surface as a size-only marker.
type wsAssembler struct {
	buf      bytes.Buffer
	opcode   byte
	inflight bool
	overflow bool
}

// frame ingests one data frame (text, binary, or continuation) and
// returns the audit body — or "" while an in-flight message has not
// yet seen its FIN frame.
func (a *wsAssembler) frame(opcode byte, fin bool, payload []byte, payloadLen uint64) string {
	switch opcode {
	case wsOpText, wsOpBinary:
		if payload == nil {
			return fmt.Sprintf("[%s %d bytes]", wsOpName(opcode), payloadLen)
		}
		if fin {
			return wsRender(opcode, payload)
		}
		a.opcode = opcode
		a.inflight = true
		a.overflow = false
		a.buf.Reset()
		a.append(payload)
		return ""

	case wsOpContinuation:
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
			return fmt.Sprintf("[%s %d bytes (assembly overflow)]", wsOpName(op), rawLen)
		}
		return wsRender(op, body)
	}
	return ""
}

func (a *wsAssembler) append(payload []byte) {
	if a.overflow || payload == nil {
		return
	}
	if a.buf.Len()+len(payload) > wsAuditLimit {
		a.overflow = true
		return
	}
	a.buf.Write(payload)
}

func wsRender(opcode byte, payload []byte) string {
	if opcode == wsOpText {
		return string(payload)
	}
	return fmt.Sprintf("[binary %d bytes]", len(payload))
}

func wsRenderClose(payload []byte) string {
	if len(payload) < 2 {
		return "[close]"
	}
	code := binary.BigEndian.Uint16(payload[:2])
	if len(payload) > 2 {
		return fmt.Sprintf("[close %d %s]", code, payload[2:])
	}
	return fmt.Sprintf("[close %d]", code)
}

// WebSocket frame directions reported as the audit chunk's `label`
// so consumers can distinguish client→upstream from upstream→client.
const (
	wsDirUp   = "up"   // client → upstream
	wsDirDown = "down" // upstream → client
)

// wsForward pumps WebSocket frames from src to dst, emitting one
// stream_chunk audit event per fully-assembled message. The dir
// ("up" or "down") is carried on each event's Label field rather
// than inlined in the body, keeping payload bytes pristine. The
// proxy strips Sec-WebSocket-Extensions from every upgrade, so
// frames on the wire are uncompressed and the recorded payload is
// exactly the application data. A frame with RSV1=1 in this regime
// indicates the server violated the stripped negotiation; we log
// and keep forwarding.
func (p *Proxy) wsForward(src io.Reader, dst io.Writer, dir string, ac *auditCtx) {
	var asm wsAssembler
	for {
		opcode, rsv1, fin, payload, payloadLen, err := wsForwardFrame(src, dst)
		if err != nil {
			log.Printf("ws frame error: host=%s dir=%s op=%d err=%v", ac.host, dir, opcode, err)
			return
		}
		if rsv1 {
			log.Printf("ws protocol violation: host=%s dir=%s op=%d rsv1=1 with stripped extensions", ac.host, dir, opcode)
		}

		var body string
		switch opcode {
		case wsOpText, wsOpBinary, wsOpContinuation:
			body = asm.frame(opcode, fin, payload, payloadLen)
		case wsOpClose:
			body = wsRenderClose(payload)
		case wsOpPing:
			body = "[ping]"
		case wsOpPong:
			body = "[pong]"
		}

		log.Printf("ws frame: host=%s dir=%s op=%d rsv1=%v fin=%v payloadLen=%d body=%q", ac.host, dir, opcode, rsv1, fin, payloadLen, body)
		if body != "" {
			ac.streamChunk(body, dir)
		}
	}
}

func wsOpName(op byte) string {
	switch op {
	case wsOpText:
		return "text"
	case wsOpBinary:
		return "binary"
	case wsOpContinuation:
		return "continuation"
	default:
		return fmt.Sprintf("op%d", op)
	}
}
