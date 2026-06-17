// Package vsockfile defines the framed wire protocol that carries a single
// file operation between the host (the microvm isolation FileBridge) and the
// in-guest agent (cmd/sbxguest) over a Firecracker vsock stream.
//
// The microvm guest sees the whole workload filesystem at its real agent
// paths — the overlay root plus the 9p-mounted workspaces — so the host
// serves every /v1/file* request by proxying it to the guest rather than
// reaching into host-side backend dirs. That keeps one code path for all
// paths instead of special-casing workspace mounts versus the guest overlay.
//
// One operation per connection. The host opens with a Request frame; list and
// stat get a single Result frame back; read/write stream the file body as Data
// frames terminated by an End frame, with a Result frame carrying the size or
// error.
//
//	+--------+------------------+-----------------+
//	| type   | length (uint32)  | payload (bytes) |
//	| 1 byte | 4 bytes, big-end | length bytes    |
//	+--------+------------------+-----------------+
package vsockfile

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// GuestPort is the vsock port the in-guest file service listens on. It sits
// next to the exec port (1024) so a host that can reach exec can reach files.
const GuestPort uint32 = 1025

// Op selects the file operation a Request performs.
type Op string

const (
	OpList   Op = "list"   // children of a directory
	OpStat   Op = "stat"   // metadata for a single entry
	OpRead   Op = "read"   // stream a regular file's bytes back
	OpWrite  Op = "write"  // create Path/Name from streamed bytes
	OpDelete Op = "delete" // remove a file or empty directory at Path
)

// Request is the opening frame (FrameRequest) the host sends.
type Request struct {
	Op   Op     `json:"op"`
	Path string `json:"path"`           // list/stat/read: the target; write: the directory
	Name string `json:"name,omitempty"` // write: basename created under Path
}

// Entry is one filesystem entry returned by list/stat.
type Entry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// Result is the terminal frame for an operation's reply direction. A non-empty
// Err signals failure and no body follows.
type Result struct {
	Err     string  `json:"err,omitempty"`
	Entries []Entry `json:"entries,omitempty"` // list
	Entry   *Entry  `json:"entry,omitempty"`   // stat
	Size    int64   `json:"size,omitempty"`    // read: file size; write: bytes written
}

// FrameType identifies the payload of a frame.
type FrameType byte

const (
	FrameRequest FrameType = 0x00 // host→guest: JSON Request (first frame)
	FrameResult  FrameType = 0x01 // guest→host: JSON Result
	FrameData    FrameType = 0x02 // either direction: raw file bytes (read/write body)
	FrameEnd     FrameType = 0x03 // either direction: end of the data stream (no payload)
)

// maxFrame caps a single frame's payload to guard against a corrupt length
// header allocating unbounded memory. Bodies larger than this are chunked.
const maxFrame = 1 << 20 // 1 MiB

// WriteFrame writes a single typed frame to w.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("vsockfile: frame too large (%d > %d)", len(payload), maxFrame)
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

// ReadFrame reads one frame from r. It returns io.EOF cleanly when the stream
// ends on a frame boundary.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("vsockfile: frame length %d exceeds max %d", n, maxFrame)
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return FrameType(hdr[0]), payload, nil
}

// ChunkSize is the data-frame payload size used when streaming a file body.
const ChunkSize = 256 * 1024
