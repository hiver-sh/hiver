package events

import (
	"context"
	"testing"
	"time"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

func makeEvent(id int64, ts time.Time) gen.SandboxEvent {
	var ev gen.SandboxEvent
	stdout := "out"
	_ = ev.FromStdioEvent(gen.StdioEvent{Id: int(id), Timestamp: ts, Stdout: &stdout})
	return ev
}

func makeExecEvent(id int64, ts time.Time) gen.SandboxEvent {
	var ev gen.SandboxEvent
	_ = ev.FromExecRequestEvent(gen.ExecRequestEvent{Id: int(id), Timestamp: ts, Cwd: "/", Command: "true"})
	return ev
}

func TestBrokerPublishFanout(t *testing.T) {
	b := New(10, 8)
	_, ch, cancel := b.Subscribe(0)
	defer cancel()

	id := b.Publish(makeEvent)
	if id != 1 {
		t.Errorf("got id %d, want 1", id)
	}

	select {
	case e := <-ch:
		if e.ID != 1 {
			t.Errorf("entry id %d, want 1", e.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for published event")
	}
}

func TestBrokerReplayAfter(t *testing.T) {
	b := New(10, 8)
	b.Publish(makeEvent) // id=1
	b.Publish(makeEvent) // id=2
	b.Publish(makeEvent) // id=3

	replay, _, cancel := b.Subscribe(1)
	defer cancel()

	if len(replay) != 2 {
		t.Fatalf("replay len %d, want 2", len(replay))
	}
	if replay[0].ID != 2 || replay[1].ID != 3 {
		t.Errorf("replay ids %d %d, want 2 3", replay[0].ID, replay[1].ID)
	}
}

func TestBrokerReplayAll(t *testing.T) {
	b := New(10, 8)
	b.Publish(makeEvent) // id=1
	b.Publish(makeEvent) // id=2

	replay, _, cancel := b.Subscribe(0)
	defer cancel()

	if len(replay) != 2 {
		t.Fatalf("replay len %d, want 2", len(replay))
	}
}

func TestBrokerRingBufferCap(t *testing.T) {
	cap := 3
	b := New(cap, 8)
	for i := 0; i < 5; i++ {
		b.Publish(makeEvent)
	}
	// ring holds ids 3, 4, 5
	replay, _, cancel := b.Subscribe(0)
	defer cancel()

	if len(replay) != cap {
		t.Fatalf("replay len %d, want %d", len(replay), cap)
	}
	if replay[0].ID != 3 {
		t.Errorf("oldest id %d, want 3", replay[0].ID)
	}
	if replay[cap-1].ID != 5 {
		t.Errorf("newest id %d, want 5", replay[cap-1].ID)
	}
}

func TestBrokerSubscriberCount(t *testing.T) {
	b := New(10, 8)
	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("want 0, got %d", n)
	}

	_, _, c1 := b.Subscribe(0)
	_, _, c2 := b.Subscribe(0)
	if n := b.SubscriberCount(); n != 2 {
		t.Fatalf("want 2, got %d", n)
	}

	c1()
	if n := b.SubscriberCount(); n != 1 {
		t.Fatalf("after cancel want 1, got %d", n)
	}

	c2()
	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("after both cancel want 0, got %d", n)
	}
}

func TestBrokerCancelClosesChannel(t *testing.T) {
	b := New(10, 8)
	_, ch, cancel := b.Subscribe(0)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: channel not closed after cancel")
	}
}

func TestBrokerClose(t *testing.T) {
	b := New(10, 8)
	_, ch1, _ := b.Subscribe(0)
	_, ch2, _ := b.Subscribe(0)
	b.Close()

	for _, ch := range []<-chan Entry{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Error("channel should be closed after broker Close")
			}
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for channel close")
		}
	}
}

func TestBrokerCloseIdempotent(t *testing.T) {
	b := New(10, 8)
	b.Close()
	b.Close() // must not panic
}

func TestBrokerSubscribeAfterClose(t *testing.T) {
	b := New(10, 8)
	b.Publish(makeEvent)
	b.Close()

	replay, ch, cancel := b.Subscribe(0)
	defer cancel()

	if len(replay) != 0 {
		t.Errorf("want no replay after Close, got %d events", len(replay))
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be immediately closed")
		}
	default:
		t.Error("channel should be closed (not blocking)")
	}
}

func TestBrokerActivityHook(t *testing.T) {
	b := New(10, 8)
	var calls int
	b.SetActivityHook(func() { calls++ })

	b.Publish(makeEvent)
	b.Publish(makeEvent)
	if calls != 2 {
		t.Errorf("Publish: hook called %d times, want 2", calls)
	}

	b.PublishSilent(makeEvent)
	if calls != 2 {
		t.Errorf("PublishSilent: hook called %d times, want still 2", calls)
	}
}

func TestBrokerWaitDrained(t *testing.T) {
	b := New(10, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// No subscribers, no publishes — should drain immediately.
	if err := b.WaitDrained(ctx, 100*time.Millisecond); err != nil {
		t.Fatalf("WaitDrained: %v", err)
	}
}

func TestBrokerWaitDrainedContextCancel(t *testing.T) {
	b := New(10, 8)
	_, _, unsub := b.Subscribe(0)
	defer unsub()

	// Publish one event first so lastPublishAt is non-zero before WaitDrained starts.
	b.Publish(makeEvent)

	// Keep publishing to prevent quietFor from being satisfied.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				b.Publish(makeEvent)
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	defer close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := b.WaitDrained(ctx, 5*time.Second); err == nil {
		t.Error("want context error, got nil")
	}
}

func TestBrokerPublishAfterClose(t *testing.T) {
	b := New(10, 8)
	b.Close()
	// Publish after close should not panic and should still increment id.
	id := b.Publish(makeEvent)
	if id != 1 {
		t.Errorf("got id %d, want 1", id)
	}
}

func TestBrokerFilterObservesOnlyListedTypes(t *testing.T) {
	b := New(10, 8)
	b.SetFilter([]string{"exec.request"})
	_, ch, cancel := b.Subscribe(0)
	defer cancel()

	b.Publish(makeEvent)     // stdio, filtered out
	b.Publish(makeExecEvent) // exec.request, observed

	select {
	case e := <-ch:
		typ, err := e.Event.Discriminator()
		if err != nil || typ != "exec.request" {
			t.Fatalf("got type %q err %v, want exec.request", typ, err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for observed event")
	}

	select {
	case e := <-ch:
		t.Fatalf("got unexpected second event id=%d, filtered type should not fan out", e.ID)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBrokerFilterEmptyObservesNothing(t *testing.T) {
	b := New(10, 8)
	b.SetFilter([]string{})
	replayBefore, ch, cancel := b.Subscribe(0)
	defer cancel()
	if len(replayBefore) != 0 {
		t.Fatalf("replay len %d, want 0", len(replayBefore))
	}

	id := b.Publish(makeEvent)
	if id != 1 {
		t.Errorf("got id %d, want 1 (ids still allocate while filtered)", id)
	}

	select {
	case e := <-ch:
		t.Fatalf("got unexpected event id=%d, empty filter should observe nothing", e.ID)
	case <-time.After(50 * time.Millisecond):
	}

	replay, _, cancel2 := b.Subscribe(0)
	cancel2()
	if len(replay) != 0 {
		t.Fatalf("replay after publish len %d, want 0 (filtered events aren't stored)", len(replay))
	}
}

func TestBrokerFilterPreservesIDsForCorrelation(t *testing.T) {
	b := New(10, 8)
	b.SetFilter([]string{"exec.response"}) // exec.request filtered out
	_, ch, cancel := b.Subscribe(0)
	defer cancel()

	firstID := b.Publish(makeExecEvent)  // filtered, still allocates an id
	secondID := b.Publish(makeExecEvent) // filtered too
	if secondID != firstID+1 {
		t.Fatalf("ids not monotonic across filtered publishes: %d then %d", firstID, secondID)
	}

	select {
	case e := <-ch:
		t.Fatalf("got unexpected event id=%d, exec.request should be filtered", e.ID)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBrokerSetFilterNilObservesEverything(t *testing.T) {
	b := New(10, 8)
	b.SetFilter([]string{"exec.request"})
	if b.Observes("stdio") {
		t.Fatal("stdio should not be observed while filter is set")
	}
	b.SetFilter(nil)
	if !b.Observes("stdio") {
		t.Fatal("stdio should be observed after clearing the filter")
	}
}

func TestBrokerMultipleSubscribersReceiveAll(t *testing.T) {
	b := New(10, 8)
	_, ch1, c1 := b.Subscribe(0)
	_, ch2, c2 := b.Subscribe(0)
	defer c1()
	defer c2()

	b.Publish(makeEvent)
	b.Publish(makeEvent)

	for _, ch := range []<-chan Entry{ch1, ch2} {
		count := 0
		for count < 2 {
			select {
			case <-ch:
				count++
			case <-time.After(time.Second):
				t.Fatalf("timeout: subscriber got %d/2 events", count)
			}
		}
	}
}
