package setup

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// WSEchoPort is the port the WebSocket echo server listens on.
// Pinned so the fixture's spec.yaml can reference it literally.
const WSEchoPort = 17082

// StartWSEchoServer binds a plain-HTTP WebSocket echo server on WSEchoPort
// (all interfaces) and returns a stop function. The server accepts WebSocket
// upgrade requests, responds with 101, then echoes every frame back to the
// client as an unmasked server frame. Frames with opcode 0x8 (Close) shut
// down the per-connection goroutine.
func StartWSEchoServer(t *testing.T) (stop func()) {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", WSEchoPort))
	if err != nil {
		t.Fatalf("ws echo: listen :%d: %v", WSEchoPort, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected WebSocket upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacker unavailable", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")

		src := io.MultiReader(brw.Reader, conn)
		for {
			opcode, payload, err := wsReadFrame(src)
			if err != nil || opcode == 0x08 { // EOF or Close frame
				return
			}
			if err := wsWriteFrame(conn, opcode, payload); err != nil {
				return
			}
		}
	})}
	go func() { _ = srv.Serve(l) }()
	return func() { _ = srv.Close() }
}

// wsReadFrame reads one WebSocket frame from r, unmasks the payload if
// masked, and returns (opcode, unmasked-payload, error).
func wsReadFrame(r io.Reader) (opcode byte, payload []byte, err error) {
	hdr := make([]byte, 2)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	pl := int(hdr[1] & 0x7F)

	switch pl {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return opcode, nil, err
		}
		pl = int(ext[0])<<8 | int(ext[1])
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return opcode, nil, err
		}
		for _, b := range ext {
			pl = pl<<8 | int(b)
		}
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(r, maskKey[:]); err != nil {
			return opcode, nil, err
		}
	}
	payload = make([]byte, pl)
	if _, err = io.ReadFull(r, payload); err != nil {
		return opcode, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload, nil
}

// wsWriteFrame writes an unmasked WebSocket frame to w.
func wsWriteFrame(w io.Writer, opcode byte, payload []byte) error {
	pl := len(payload)
	var hdr []byte
	hdr = append(hdr, 0x80|opcode) // FIN=1
	switch {
	case pl < 126:
		hdr = append(hdr, byte(pl))
	case pl < 65536:
		hdr = append(hdr, 126, byte(pl>>8), byte(pl))
	default:
		hdr = append(hdr, 127,
			byte(pl>>56), byte(pl>>48), byte(pl>>40), byte(pl>>32),
			byte(pl>>24), byte(pl>>16), byte(pl>>8), byte(pl))
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
