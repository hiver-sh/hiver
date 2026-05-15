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
// it invokes onExpire exactly once and returns.
func (l *Lifetime) Run(ctx context.Context) {
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
