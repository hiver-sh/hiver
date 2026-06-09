package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestLifetime_ExpiresAfterTTL(t *testing.T) {
	var expired atomic.Bool
	ttl := 100 * time.Millisecond
	l := NewLifetime(func() time.Duration { return ttl }, func() { expired.Store(true) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after TTL expired")
	}
	if !expired.Load() {
		t.Error("onExpire was not called")
	}
}

func TestLifetime_ResetExtends(t *testing.T) {
	var expired atomic.Bool
	ttl := 200 * time.Millisecond
	l := NewLifetime(func() time.Duration { return ttl }, func() { expired.Store(true) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()

	// Reset several times within the TTL window to extend the deadline.
	for i := 0; i < 4; i++ {
		time.Sleep(80 * time.Millisecond)
		if expired.Load() {
			t.Error("expired too early")
			return
		}
		l.Reset()
	}

	// Now let it expire naturally.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after TTL expired post-reset")
	}
	if !expired.Load() {
		t.Error("onExpire was not called after resets")
	}
}

func TestLifetime_ContextCancelStops(t *testing.T) {
	var expired atomic.Bool
	ttl := 10 * time.Second
	l := NewLifetime(func() time.Duration { return ttl }, func() { expired.Store(true) })

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if expired.Load() {
		t.Error("onExpire should not be called on ctx cancel")
	}
}

func TestLifetime_ZeroTTLSkipsExpiry(t *testing.T) {
	var expired atomic.Bool
	l := NewLifetime(func() time.Duration { return 0 }, func() { expired.Store(true) })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()

	<-done
	if expired.Load() {
		t.Error("onExpire should not fire when ttl=0")
	}
}
