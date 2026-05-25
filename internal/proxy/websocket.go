package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
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

// wsAuditLimit is the maximum payload buffered in memory for audit logging.
// Frames larger than this are streamed without buffering; the audit event
// records the size rather than the content.
const wsAuditLimit = 64 * 1024

// wsForwardFrame copies exactly one WebSocket frame from src to dst.
// It returns the opcode, the unmasked payload (nil when the frame exceeds
// wsAuditLimit or a read/write error occurs mid-frame), and the raw payload
// length. err is non-nil only when the frame could not be transferred in full.
//
// Frame bytes are written to dst incrementally as they are read, so dst
// always receives the complete frame wire representation (including the
// original mask key and masked payload bytes).
func wsForwardFrame(src io.Reader, dst io.Writer) (opcode byte, payload []byte, payloadLen uint64, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(src, hdr[:]); err != nil {
		return 0, nil, 0, err
	}
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	pl := uint64(hdr[1] & 0x7F)

	if _, err = dst.Write(hdr[:]); err != nil {
		return opcode, nil, 0, err
	}

	switch pl {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(src, ext[:]); err != nil {
			return opcode, nil, 0, err
		}
		if _, err = dst.Write(ext[:]); err != nil {
			return opcode, nil, 0, err
		}
		pl = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(src, ext[:]); err != nil {
			return opcode, nil, 0, err
		}
		if _, err = dst.Write(ext[:]); err != nil {
			return opcode, nil, 0, err
		}
		pl = binary.BigEndian.Uint64(ext[:])
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(src, maskKey[:]); err != nil {
			return opcode, nil, pl, err
		}
		if _, err = dst.Write(maskKey[:]); err != nil {
			return opcode, nil, pl, err
		}
	}

	// Large frames: stream without buffering; the caller notes the size.
	if pl > wsAuditLimit {
		_, err = io.CopyN(dst, src, int64(pl))
		return opcode, nil, pl, err
	}

	data := make([]byte, pl)
	if _, err = io.ReadFull(src, data); err != nil {
		_, _ = dst.Write(data)
		return opcode, nil, pl, err
	}
	if _, err = dst.Write(data); err != nil {
		return opcode, nil, pl, err
	}
	if masked {
		for i := range data {
			data[i] ^= maskKey[i%4]
		}
	}
	return opcode, data, pl, nil
}

// wsForward pumps WebSocket frames from src to dst, emitting one
// stream_chunk audit event per fully-transferred frame. It returns when src
// signals EOF or any read/write error occurs.
func (p *Proxy) wsForward(src io.Reader, dst io.Writer, ac *auditCtx) {
	for {
		opcode, payload, payloadLen, err := wsForwardFrame(src, dst)

		// Only emit an audit event when the frame was fully transferred.
		// On a header-read failure wsForwardFrame returns opcode=0 (the zero
		// byte), which is the same value as a real continuation frame — without
		// this guard we'd emit a spurious "[continuation 0 bytes]" on close.
		if err == nil {
			var body string
			switch opcode {
			case wsOpText:
				if payload != nil {
					body = string(payload)
				} else {
					body = fmt.Sprintf("[text %d bytes]", payloadLen)
				}
			case wsOpBinary:
				body = fmt.Sprintf("[binary %d bytes]", payloadLen)
			case wsOpContinuation:
				body = fmt.Sprintf("[continuation %d bytes]", payloadLen)
			case wsOpClose:
				if len(payload) >= 2 {
					code := binary.BigEndian.Uint16(payload[:2])
					if len(payload) > 2 {
						body = fmt.Sprintf("[close %d %s]", code, payload[2:])
					} else {
						body = fmt.Sprintf("[close %d]", code)
					}
				} else {
					body = "[close]"
				}
			case wsOpPing:
				body = "[ping]"
			case wsOpPong:
				body = "[pong]"
			}
			log.Printf("ws frame: host=%s op=%d payloadLen=%d body=%q", ac.host, opcode, payloadLen, body)
			if body != "" {
				ac.streamChunk(body)
			}
		} else {
			log.Printf("ws frame error: host=%s op=%d err=%v", ac.host, opcode, err)
		}
		if err != nil {
			return
		}
	}
}
