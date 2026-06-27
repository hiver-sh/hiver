// Package vsockexec defines the framed wire protocol that carries an exec
// session between the host bridge (cmd/sbxvsock) and the in-guest agent
// (cmd/sbxguest) over a Firecracker vsock stream.
//
// A session is a sequence of length-prefixed frames in both directions:
//
//	+--------+------------------+-----------------+
//	| type   | length (uint32)  | payload (bytes) |
//	| 1 byte | 4 bytes, big-end | length bytes    |
//	+--------+------------------+-----------------+
//
// The host opens with a single Start frame describing the command, then
// streams Stdin frames; the guest replies with Stdout/Stderr frames and a
// terminal Exit frame carrying the process exit code. Framing (rather than
// a raw byte stream) lets a single connection multiplex stdout/stderr and
// deliver the exit status in-band, which a raw pipe cannot.
package vsockexec

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// FrameType identifies the payload of a frame.
type FrameType byte

const (
	FrameStart      FrameType = 0x00 // host→guest: JSON-encoded Start
	FrameStdin      FrameType = 0x01 // host→guest: raw stdin bytes
	FrameStdout     FrameType = 0x02 // guest→host: raw stdout bytes
	FrameStderr     FrameType = 0x03 // guest→host: raw stderr bytes
	FrameResize     FrameType = 0x04 // host→guest: JSON-encoded Winsize (tty only)
	FrameExit       FrameType = 0x05 // guest→host: JSON-encoded Exit (terminal)
	FrameStdinClose FrameType = 0x06 // host→guest: stdin reached EOF
)

// maxFrame caps a single frame's payload to guard against a corrupt length
// header allocating unbounded memory.
const maxFrame = 1 << 20 // 1 MiB

// Start is the opening frame describing the command to run in the guest.
type Start struct {
	Command string            `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	TTY     bool              `json:"tty,omitempty"`
	Rows    uint16            `json:"rows,omitempty"`
	Cols    uint16            `json:"cols,omitempty"`

	// SessionID, when non-empty, names a DETACHABLE session: the guest agent
	// keeps the process + pty alive across a dropped connection (a snapshot
	// resume cuts the host's exec TCP), and a later Start with the same id
	// re-attaches the live process instead of launching a new one. The entrypoint
	// tty uses a fixed id so a resume re-attaches warm rather than relaunching.
	// An empty id is an ordinary one-shot exec (reaped when the connection ends).
	SessionID string `json:"session_id,omitempty"`
}

// Winsize is the payload of a FrameResize frame.
type Winsize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Exit is the payload of the terminal FrameExit frame.
type Exit struct {
	Code int `json:"code"`
}

// WriteFrame writes a single typed frame to w.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("vsockexec: frame too large (%d > %d)", len(payload), maxFrame)
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// WriteJSON marshals v and writes it as a frame of type t.
func WriteJSON(w io.Writer, t FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, b)
}

// ReadFrame reads one frame from r. It returns io.EOF cleanly when the
// stream ends on a frame boundary.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("vsockexec: frame length %d exceeds max %d", n, maxFrame)
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return FrameType(hdr[0]), payload, nil
}
