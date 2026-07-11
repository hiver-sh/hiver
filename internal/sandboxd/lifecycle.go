package sandboxd

import (
	"sync"
	"time"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// lifecycleBroker fans pod-level inner-sandbox lifecycle transitions out to
// subscribers — one per open GET /v1/events stream (the controller holds one
// such connection per pod). Delivery is best-effort: a slow subscriber drops
// events rather than blocking the publisher, since the controller re-syncs the
// authoritative set via GET /v1.
type lifecycleBroker struct {
	mu   sync.Mutex
	subs map[int]chan gen.PodEvent
	next int
}

func newLifecycleBroker() *lifecycleBroker {
	return &lifecycleBroker{subs: map[int]chan gen.PodEvent{}}
}

// publish delivers a transition for key to every current subscriber.
func (b *lifecycleBroker) publish(key string, status gen.PodEventStatus) {
	ev := gen.PodEvent{Key: key, Status: status, Timestamp: time.Now()}
	b.mu.Lock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow subscriber — drop rather than block
		}
	}
	b.mu.Unlock()
}

// Subscribe registers a subscriber, returning its channel and an unsubscribe
// func. Satisfies handlers.Supervisor.
func (b *lifecycleBroker) Subscribe() (<-chan gen.PodEvent, func()) {
	b.mu.Lock()
	id := b.next
	b.next++
	ch := make(chan gen.PodEvent, 64)
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}
