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

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// DefaultCapacity is the number of recent events the broker keeps for
// `lastEventId` replay. Older events fall off the back of the ring.
const DefaultCapacity = 500

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
	activityHook  func()    // called by Publish (not PublishSilent) outside the lock

	// filter restricts which event types are stored/fanned out, per
	// SandboxConfig.events. nil (the default) observes every type. A non-nil
	// filter observes exactly the types it contains — an empty-but-non-nil
	// filter observes nothing. See SetFilter.
	filter map[string]struct{}
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

// SetActivityHook registers fn to be called on every Publish (but not
// PublishSilent). fn is invoked outside the broker's lock; it must be
// set before the first Publish call to avoid data races.
func (b *Broker) SetActivityHook(fn func()) {
	b.activityHook = fn
}

// SetFilter restricts which event types this broker stores and fans out.
// Pass nil to observe every type (the default). Pass a non-nil slice to
// observe only the listed types — an empty (but non-nil) slice observes
// nothing. Safe to call at any time, including after publishing has
// started (a config apply can narrow or widen the filter live); a type
// excluded mid-stream simply stops appearing in subsequent Publish calls.
//
// Filtering happens after `build` runs, inside publish's critical section:
// every call still allocates a unique, monotonic id (so a filtered-out
// event's id can still be referenced by a later, observed, correlated
// event — e.g. an ingress.response's request_id when ingress.request
// itself isn't observed), it just isn't stored or fanned out.
func (b *Broker) SetFilter(types []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if types == nil {
		b.filter = nil
		return
	}
	m := make(map[string]struct{}, len(types))
	for _, t := range types {
		m[t] = struct{}{}
	}
	b.filter = m
}

// Observes reports whether eventType is currently observed — i.e. whether a
// Publish/PublishSilent call for that type would be stored and fanned out.
// Callers doing non-trivial work to build an event (e.g. capturing a request
// body) should check this first and skip that work entirely when it would be
// filtered out anyway.
func (b *Broker) Observes(eventType string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observesLocked(eventType)
}

func (b *Broker) observesLocked(eventType string) bool {
	if b.filter == nil {
		return true
	}
	_, ok := b.filter[eventType]
	return ok
}

// FilterFromConfig converts SandboxConfig.Events (nil when the field is
// unset) to the []string SetFilter expects, preserving the nil-means-observe-
// everything / non-nil-means-restrict distinction.
func FilterFromConfig(types *[]gen.EventType) []string {
	if types == nil {
		return nil
	}
	out := make([]string, len(*types))
	for i, t := range *types {
		out[i] = string(t)
	}
	return out
}

// Publish allocates the next id+timestamp, hands them to `build` to
// construct the event variant, stores the result in the ring, and fans
// it out to subscribers. Non-blocking sends mean a stuck subscriber
// can't stall publishers (gaps are filled by ring replay on reconnect).
// Returns the assigned id. Publish calls the activity hook (if set);
// use PublishSilent for events that should not reset the inactivity timer.
func (b *Broker) Publish(build Factory) int64 {
	id := b.publish(build, true)
	if fn := b.activityHook; fn != nil {
		fn()
	}
	return id
}

// PublishSilent is like Publish but does not trigger the activity hook.
// Use it for background telemetry events (e.g. resource.usage) that
// should not reset the sandbox inactivity timer.
func (b *Broker) PublishSilent(build Factory) int64 {
	return b.publish(build, true)
}

func (b *Broker) publish(build Factory, store bool) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	now := time.Now().UTC()
	entry := Entry{ID: b.nextID, Event: build(b.nextID, now)}
	b.lastPublishAt = now
	if typ, err := entry.Event.Discriminator(); err != nil || b.observesLocked(typ) {
		if store {
			if len(b.ring) < b.cap {
				b.ring = append(b.ring, entry)
			} else {
				copy(b.ring, b.ring[1:])
				b.ring[b.cap-1] = entry
			}
		}
		for _, ch := range b.subs {
			select {
			case ch <- entry:
			default:
			}
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
