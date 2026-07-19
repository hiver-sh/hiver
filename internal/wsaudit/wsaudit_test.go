package wsaudit

import (
	"net/http"
	"strings"
	"testing"
)

func TestAssembler_UnfragmentedText(t *testing.T) {
	var a assembler
	got := a.frame(OpText, true, []byte("hello"), 5)
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
	if a.inflight {
		t.Fatal("inflight should be false after FIN")
	}
}

func TestAssembler_UnfragmentedBinary(t *testing.T) {
	var a assembler
	got := a.frame(OpBinary, true, []byte{1, 2, 3, 4, 5}, 5)
	if got != "[binary 5 bytes]" {
		t.Fatalf("got %q", got)
	}
}

func TestAssembler_FragmentedTextReassembles(t *testing.T) {
	var a assembler
	if b := a.frame(OpText, false, []byte("hel"), 3); b != "" {
		t.Fatalf("first fragment should emit nothing, got %q", b)
	}
	if !a.inflight {
		t.Fatal("expected inflight after first fragment")
	}
	if b := a.frame(OpContinuation, false, []byte("lo "), 3); b != "" {
		t.Fatalf("mid fragment should emit nothing, got %q", b)
	}
	got := a.frame(OpContinuation, true, []byte("world"), 5)
	if got != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
	if a.inflight {
		t.Fatal("inflight should be false after FIN continuation")
	}
}

func TestAssembler_StrayContinuation(t *testing.T) {
	var a assembler
	got := a.frame(OpContinuation, true, []byte("hi"), 2)
	if got != "[continuation 2 bytes]" {
		t.Fatalf("got %q", got)
	}
}

func TestAssembler_OversizedFrame(t *testing.T) {
	var a assembler
	// payload == nil simulates a frame larger than AuditLimit.
	got := a.frame(OpText, true, nil, AuditLimit*2)
	want := "[text " + itoa(AuditLimit*2) + " bytes]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAssembler_AssemblyOverflowMarker(t *testing.T) {
	var a assembler
	// First fragment: half the limit.
	half := make([]byte, AuditLimit/2)
	a.frame(OpText, false, half, uint64(len(half)))
	// Continuation that would push over the limit.
	rest := make([]byte, AuditLimit) // 1.5x total
	got := a.frame(OpContinuation, true, rest, uint64(len(rest)))
	if !strings.HasPrefix(got, "[text ") || !strings.HasSuffix(got, "(assembly overflow)]") {
		t.Fatalf("got %q, want overflow marker", got)
	}
}

func TestAssembler_BackToBackMessages(t *testing.T) {
	var a assembler
	if b := a.frame(OpText, true, []byte("first"), 5); b != "first" {
		t.Fatalf("got %q", b)
	}
	if b := a.frame(OpText, true, []byte("second"), 6); b != "second" {
		t.Fatalf("got %q", b)
	}
}

func TestStripExtensions_Header(t *testing.T) {
	h := http.Header{}
	h.Set("Sec-WebSocket-Extensions", "permessage-deflate; client_max_window_bits")
	h.Set("Authorization", "Bearer x")
	StripExtensions(h)
	if v := h.Get("Sec-Websocket-Extensions"); v != "" {
		t.Fatalf("extensions header still present: %q", v)
	}
	if h.Get("Authorization") != "Bearer x" {
		t.Fatal("unrelated header was disturbed")
	}
}

func TestStripExtensionsRaw_Removes(t *testing.T) {
	in := []byte(
		"GET /chat HTTP/1.1\r\n" +
			"Host: example.com\r\n" +
			"Sec-WebSocket-Extensions: permessage-deflate\r\n" +
			"Authorization: Bearer xyz\r\n" +
			"\r\n",
	)
	out := StripExtensionsRaw(in)
	if strings.Contains(string(out), "Sec-WebSocket-Extensions") {
		t.Fatalf("header not stripped:\n%s", out)
	}
	if !strings.Contains(string(out), "Authorization: Bearer xyz") {
		t.Fatalf("unrelated header disturbed:\n%s", out)
	}
	if !strings.HasSuffix(string(out), "\r\n\r\n") {
		t.Fatalf("header terminator lost:\n%q", out)
	}
}

func TestStripExtensionsRaw_CaseInsensitive(t *testing.T) {
	in := []byte(
		"GET / HTTP/1.1\r\n" +
			"sec-websocket-extensions: permessage-deflate\r\n" +
			"\r\n",
	)
	out := StripExtensionsRaw(in)
	if strings.Contains(strings.ToLower(string(out)), "sec-websocket-extensions") {
		t.Fatalf("lowercase header not stripped:\n%s", out)
	}
}

func TestStripExtensionsRaw_NoHeaderUnchanged(t *testing.T) {
	in := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	out := StripExtensionsRaw(in)
	if &out[0] != &in[0] {
		t.Fatal("expected the original slice back when no rewrite needed")
	}
}

// itoa is a stdlib-free integer formatter for test labels so we can
// keep the test imports minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
