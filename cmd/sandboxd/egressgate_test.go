package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A waiter is released once sbxproxy acks a generation >= its own.
func TestEgressGateWaitReleasedByAck(t *testing.T) {
	g := newEgressGate()
	gen := g.bumpDesired()

	done := make(chan struct{})
	go func() {
		g.waitApplied(context.Background(), gen)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("waitApplied returned before the generation was acked")
	case <-time.After(20 * time.Millisecond):
	}

	g.markApplied(gen)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitApplied did not return after the ack")
	}
}

// A later ack satisfies every earlier waiter (coalescing: one reload at the
// highest generation releases all pending creates below it).
func TestEgressGateLaterAckReleasesEarlierWaiters(t *testing.T) {
	g := newEgressGate()
	g1 := g.bumpDesired()
	g2 := g.bumpDesired()
	g3 := g.bumpDesired()

	var wg sync.WaitGroup
	for _, gen := range []uint64{g1, g2, g3} {
		wg.Add(1)
		go func(gen uint64) {
			defer wg.Done()
			g.waitApplied(context.Background(), gen)
		}(gen)
	}

	// A single ack at the newest generation must release all three.
	g.markApplied(g3)

	released := make(chan struct{})
	go func() { wg.Wait(); close(released) }()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("a coalesced ack at the highest gen did not release earlier waiters")
	}
}

// markApplied is monotonic: a stale (lower) ack must not lower the watermark or
// strand a higher waiter.
func TestEgressGateMonotonic(t *testing.T) {
	g := newEgressGate()
	g.markApplied(5)
	g.markApplied(3) // stale, ignored

	done := make(chan struct{})
	go func() {
		g.waitApplied(context.Background(), 4) // already covered by the gen-5 ack
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiter <= the applied watermark was not released")
	}
}

// A cancelled context unblocks a waiter even if no ack ever arrives, so a create
// can't hang on a missed reload.
func TestEgressGateWaitCtxCancel(t *testing.T) {
	g := newEgressGate()
	gen := g.bumpDesired()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.waitApplied(ctx, gen)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitApplied did not return on context cancel")
	}
}

// signal coalesces: many bumps with the reloader not draining leave at most one
// pending wake, and the reloader always reads the latest desired generation.
func TestEgressGateSignalCoalesces(t *testing.T) {
	g := newEgressGate()
	for i := 0; i < 100; i++ {
		g.bumpDesired()
		g.signal()
	}
	// One wake is buffered; draining it once should be enough to observe the
	// final generation (the reloader reads currentDesired after each wake).
	select {
	case <-g.wake:
	default:
		t.Fatal("expected a pending wake after signalling")
	}
	if got := g.currentDesired(); got != 100 {
		t.Errorf("currentDesired = %d, want 100", got)
	}
}
