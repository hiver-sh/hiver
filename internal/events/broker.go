// Package events implements the in-memory pub/sub that backs
// `GET /v1/events`. Sidecar audit events arrive at sandboxd over a
// socketpair, get translated into a [gen.SandboxEvent] variant, and
// land here. Subscribers (one per SSE client) receive a fan-out copy
// and can resume after a known event id via the ring buffer.
package events

import (
	"context"
	"sync"
	"time"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
)

// DefaultCapacity is the number of recent events the broker keeps for
// `lastEventId` replay. Older events fall off the back of the ring.
const DefaultCapacity = 1000

// Factory builds a [gen.SandboxEvent] given the id+timestamp the broker
// just allocated. The broker calls Factory inside its critical section
// so id assignment, timestamp stamping, ring-buffer append, and fan-out
// happen atomically — no other goroutine can publish in between.
type Factory func(id int64, ts time.Time) gen.SandboxEvent

// Entry pairs a stored event with its monotonic id; needed because the
// id lives inside the variant's JSON union and would be expensive to
// re-extract during replay scans.
type Entry struct {
	ID    int64
	Event gen.SandboxEvent
}

// Broker is a goroutine-safe pub/sub with a fixed-capacity ring buffer
// for resume-after-disconnect.
type Broker struct {
	mu            sync.Mutex
	nextID        int64
	ring          []Entry // most recent up to cap, oldest first
	cap           int
	subs          map[int64]chan Entry
	nextSub       int64
	subDepth      int
	lastPublishAt time.Time // updated under mu on every Publish
	closed        bool      // Close() flips this; rejects new subscribers
}

// New returns a Broker that keeps the most recent `capacity` events for
// resume. Pass 0 to use [DefaultCapacity]. `subDepth` is the per-
// subscriber channel buffer; slow consumers beyond this depth drop
// events (clients can resume from the ring on reconnect).
func New(capacity, subDepth int) *Broker {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if subDepth <= 0 {
		subDepth = 64
	}
	return &Broker{
		cap:      capacity,
		subs:     make(map[int64]chan Entry),
		subDepth: subDepth,
	}
}

// Publish allocates the next id+timestamp, hands them to `build` to
// construct the event variant, stores the result in the ring, and fans
// it out to subscribers. Non-blocking sends mean a stuck subscriber
// can't stall publishers (gaps are filled by ring replay on reconnect).
// Returns the assigned id.
func (b *Broker) Publish(build Factory) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	now := time.Now().UTC()
	entry := Entry{ID: b.nextID, Event: build(b.nextID, now)}
	b.lastPublishAt = now
	if len(b.ring) < b.cap {
		b.ring = append(b.ring, entry)
	} else {
		copy(b.ring, b.ring[1:])
		b.ring[b.cap-1] = entry
	}
	for _, ch := range b.subs {
		select {
		case ch <- entry:
		default:
		}
	}
	return b.nextID
}

// Subscribe returns all buffered entries with id > after as a replay
// slice, plus a live channel that receives every subsequent Publish.
// The returned cancel func unsubscribes and closes the channel.
//
// Replay is returned as a plain slice (not through the bounded channel)
// so callers always receive every buffered event regardless of subDepth.
func (b *Broker) Subscribe(after int64) ([]Entry, <-chan Entry, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Entry, b.subDepth)
	if b.closed {
		// Broker is down — replay nothing, hand back a closed channel
		// so the caller exits its receive loop immediately.
		close(ch)
		return nil, ch, func() {}
	}
	var replay []Entry
	for _, e := range b.ring {
		if e.ID > after {
			replay = append(replay, e)
		}
	}
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	return replay, ch, func() { b.unsubscribe(id, ch) }
}

// SubscriberCount returns the number of live subscribers. Useful to
// short-circuit drain logic ("nobody is listening, don't bother
// waiting before shutting down").
func (b *Broker) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// WaitDrained blocks until every subscriber's channel is empty AND
// no new event has been Publish'd for at least `quietFor`, or until
// ctx expires (returning ctx.Err()).
//
// "Drained" here means the broker-side fan-out queue is empty — not
// that the bytes have reached the wire. Combined with the SSE
// handler doing a Flush after every channel receive, this is the
// strongest "we've handed everything over" signal sandboxd can offer
// before tearing the sidecars down.
//
// `quietFor` gives the audit pipeline (sidecar → translator → broker)
// time to land any in-flight events the agent emitted in its last
// moments; a too-short value would let WaitDrained return between
// publishes, before trailing events arrive.
func (b *Broker) WaitDrained(ctx context.Context, quietFor time.Duration) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if b.drained(quietFor) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (b *Broker) drained(quietFor time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.lastPublishAt.IsZero() && time.Since(b.lastPublishAt) < quietFor {
		return false
	}
	for _, ch := range b.subs {
		if len(ch) > 0 {
			return false
		}
	}
	return true
}

// Close ends the broker: closes every subscriber's channel (so SSE
// handlers blocked on receive observe ok=false and return) and
// rejects further subscriptions. Idempotent. Publishers can still
// call Publish after Close — the event is appended to the ring (so
// late subscribers could still see it on resume) but no live
// subscriber is notified.
//
// Use during graceful shutdown: drain first (so existing subscribers
// receive trailing events), then Close to release them, then
// http.Server.Shutdown to wait for the SSE handlers to fully return.
func (b *Broker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := b.subs
	b.subs = nil
	b.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
}

func (b *Broker) unsubscribe(id int64, ch chan Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[id]; !ok {
		return
	}
	delete(b.subs, id)
	close(ch)
}
