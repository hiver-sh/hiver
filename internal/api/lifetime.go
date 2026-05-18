package api

import (
	"context"
	"sync"
	"time"
)

type Lifetime struct {
	ttlFn    func() time.Duration
	onExpire func()

	mu   sync.Mutex
	last time.Time
}

func NewLifetime(ttlFn func() time.Duration, onExpire func()) *Lifetime {
	return &Lifetime{
		ttlFn:    ttlFn,
		onExpire: onExpire,
		last:     time.Now(),
	}
}

// Reset records that a ping just arrived.
func (l *Lifetime) Reset() {
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
}

// Run blocks until ctx is cancelled or the deadline elapses; on elapse
// it invokes onExpire exactly once and returns. The TTL countdown starts
// from when Run is called, not from when NewLifetime was constructed, so
// callers should call Run only once the API server is ready to accept pings.
func (l *Lifetime) Run(ctx context.Context) {
	l.Reset() // start the countdown from now, not from construction time
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			ttl := l.ttlFn()
			if ttl <= 0 {
				continue
			}
			l.mu.Lock()
			elapsed := now.Sub(l.last)
			l.mu.Unlock()
			if elapsed >= ttl {
				l.onExpire()
				return
			}
		}
	}
}
