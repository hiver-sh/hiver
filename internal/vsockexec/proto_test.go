package vsockexec

import (
	"bytes"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := []byte("hello stdout")
	if err := WriteFrame(&buf, FrameStdout, want); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	ft, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameStdout {
		t.Errorf("type = %d, want %d", ft, FrameStdout)
	}
	if !bytes.Equal(payload, want) {
		t.Errorf("payload = %q, want %q", payload, want)
	}
}

func TestEmptyFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameStdinClose, nil); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	ft, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameStdinClose || len(payload) != 0 {
		t.Errorf("got (%d, %d bytes), want (%d, 0 bytes)", ft, len(payload), FrameStdinClose)
	}
}

func TestJSONFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, FrameStart, Start{Command: "echo hi", TTY: true}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	ft, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameStart {
		t.Fatalf("type = %d, want %d", ft, FrameStart)
	}
	if string(payload) != `{"command":"echo hi","tty":true}` {
		t.Errorf("payload = %s", payload)
	}
}

func TestReadFrameCleanEOF(t *testing.T) {
	if _, _, err := ReadFrame(bytes.NewReader(nil)); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestReadFrameOversizeLength(t *testing.T) {
	// type byte + a length header of 2 MiB (over the 1 MiB cap).
	hdr := []byte{byte(FrameStdout), 0x00, 0x20, 0x00, 0x00}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); err == nil {
		t.Error("expected error for oversize frame length, got nil")
	}
}

func TestSequencedFrames(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, FrameStdin, []byte("in"))
	_ = WriteJSON(&buf, FrameExit, Exit{Code: 7})

	ft, p, _ := ReadFrame(&buf)
	if ft != FrameStdin || string(p) != "in" {
		t.Fatalf("frame 1 = (%d, %q)", ft, p)
	}
	ft, p, _ = ReadFrame(&buf)
	if ft != FrameExit {
		t.Fatalf("frame 2 type = %d", ft)
	}
	if string(p) != `{"code":7}` {
		t.Errorf("exit payload = %s", p)
	}
}
