// Package events implements the in-memory pub/sub that backs
// `GET /v1/events`. Sidecar audit events arrive at sandboxd over a
// socketpair, get translated into a [gen.SandboxEvent] variant, and
// land here. Subscribers (one per SSE client) receive a fan-out copy
// and can resume after a known event id via the ring buffer.
package events

import (
	"sync"
	"time"

	gen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/sandbox"
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
	mu       sync.Mutex
	nextID   int64
	ring     []Entry // most recent up to cap, oldest first
	cap      int
	subs     map[int64]chan Entry
	nextSub  int64
	subDepth int
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
	entry := Entry{ID: b.nextID, Event: build(b.nextID, time.Now().UTC())}
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

// Subscribe returns a channel that receives every Publish after the
// call, plus a replay of every buffered entry with id > after. The
// returned cancel func unsubscribes and closes the channel.
//
// Replay is bounded by the ring capacity; older events are lost.
func (b *Broker) Subscribe(after int64) (<-chan Entry, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Entry, b.subDepth)
	for _, e := range b.ring {
		if e.ID > after {
			select {
			case ch <- e:
			default:
				// Channel filled by replay alone; remaining buffered
				// events fall off but the live stream continues.
			}
		}
	}
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	return ch, func() { b.unsubscribe(id, ch) }
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
