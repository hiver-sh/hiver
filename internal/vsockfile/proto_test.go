package vsockfile

import (
	"bytes"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := []byte("hello world")
	if err := WriteFrame(&buf, FrameData, want); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	ft, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameData {
		t.Errorf("type = %d, want %d", ft, FrameData)
	}
	if !bytes.Equal(payload, want) {
		t.Errorf("payload = %q, want %q", payload, want)
	}
}

func TestEmptyFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameEnd, nil); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	ft, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameEnd || len(payload) != 0 {
		t.Errorf("got (%d, %d bytes), want (%d, 0)", ft, len(payload), FrameEnd)
	}
}

func TestJSONFrame(t *testing.T) {
	var buf bytes.Buffer
	req := Request{Op: OpRead, Path: "/workspace/file.txt"}
	if err := WriteJSON(&buf, FrameRequest, req); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	ft, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != FrameRequest {
		t.Fatalf("type = %d, want %d", ft, FrameRequest)
	}
	if string(payload) != `{"op":"read","path":"/workspace/file.txt"}` {
		t.Errorf("payload = %s", payload)
	}
}

func TestReadFrameCleanEOF(t *testing.T) {
	if _, _, err := ReadFrame(bytes.NewReader(nil)); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestReadFrameOversizeLength(t *testing.T) {
	// type byte + length of 2 MiB (over the 1 MiB cap)
	hdr := []byte{byte(FrameData), 0x00, 0x20, 0x00, 0x00}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); err == nil {
		t.Error("expected error for oversize frame, got nil")
	}
}

func TestWriteFrameOversizePayload(t *testing.T) {
	var buf bytes.Buffer
	large := make([]byte, maxFrame+1)
	if err := WriteFrame(&buf, FrameData, large); err == nil {
		t.Error("expected error for oversize payload, got nil")
	}
}

func TestSequencedFrames(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, FrameData, []byte("chunk1"))
	_ = WriteJSON(&buf, FrameResult, Result{Size: 6})

	ft, p, err := ReadFrame(&buf)
	if err != nil || ft != FrameData || string(p) != "chunk1" {
		t.Fatalf("frame 1 = (%d, %q, %v)", ft, p, err)
	}

	ft, p, err = ReadFrame(&buf)
	if err != nil || ft != FrameResult {
		t.Fatalf("frame 2 = (%d, %v)", ft, err)
	}
	if string(p) != `{"size":6}` {
		t.Errorf("result payload = %s", p)
	}
}

func TestAllFrameTypes(t *testing.T) {
	types := []FrameType{FrameRequest, FrameResult, FrameData, FrameEnd}
	for _, ft := range types {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, ft, nil); err != nil {
			t.Fatalf("WriteFrame type %d: %v", ft, err)
		}
		got, _, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame type %d: %v", ft, err)
		}
		if got != ft {
			t.Errorf("type round-trip: got %d, want %d", got, ft)
		}
	}
}

func TestResultWithError(t *testing.T) {
	var buf bytes.Buffer
	res := Result{Err: "file not found"}
	if err := WriteJSON(&buf, FrameResult, res); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	_, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(payload) != `{"err":"file not found"}` {
		t.Errorf("payload = %s", payload)
	}
}
