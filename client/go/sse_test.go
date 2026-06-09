package client

import (
	"strings"
	"testing"
)

func TestReadSSE_SingleEvent(t *testing.T) {
	ch := readSSE(strings.NewReader("data: hello\n\n"))
	f := <-ch
	if f.data != "hello" {
		t.Fatalf("got %q, want %q", f.data, "hello")
	}
	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed after EOF")
	}
}

func TestReadSSE_MultipleEvents(t *testing.T) {
	input := "id: 1\ndata: first\n\nid: 2\ndata: second\n\n"
	ch := readSSE(strings.NewReader(input))

	f1 := <-ch
	if f1.id != "1" || f1.data != "first" {
		t.Fatalf("event 1: id=%q data=%q", f1.id, f1.data)
	}
	f2 := <-ch
	if f2.id != "2" || f2.data != "second" {
		t.Fatalf("event 2: id=%q data=%q", f2.id, f2.data)
	}
}

func TestReadSSE_SkipsEventsWithNoData(t *testing.T) {
	// an event with only an id and no data should not be dispatched
	input := "id: 1\n\ndata: real\n\n"
	ch := readSSE(strings.NewReader(input))
	f := <-ch
	if f.data != "real" {
		t.Fatalf("got %q, want %q", f.data, "real")
	}
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel")
	}
}

func TestReadSSE_StripsLeadingSpace(t *testing.T) {
	ch := readSSE(strings.NewReader("data: with space\n\n"))
	f := <-ch
	if f.data != "with space" {
		t.Fatalf("got %q, want %q", f.data, "with space")
	}
}

func TestReadSSE_NoLeadingSpacePreserved(t *testing.T) {
	ch := readSSE(strings.NewReader("data:nospace\n\n"))
	f := <-ch
	if f.data != "nospace" {
		t.Fatalf("got %q, want %q", f.data, "nospace")
	}
}
