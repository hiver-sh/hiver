package proxy

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is an io.Writer safe for the proxy's audit goroutine to write while
// the test reads — the deny audit is emitted from the awaitEgress goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// auditProxy builds a Proxy with a captured audit sink for the deny-wait tests.
func auditProxy(t *testing.T) (*Proxy, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	p, err := New(Config{Addr: "127.0.0.1:0", Audit: buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, buf
}

func ac(p *Proxy, srcIP, method, host, path string) *auditCtx {
	return p.beginAudit(srcIP, method, host, path, "")
}

// TestAwaitEgressImmediateAllow: an allowed request returns its rule at once,
// without consulting the deny-wait timer.
func TestAwaitEgressImmediateAllow(t *testing.T) {
	p, _ := auditProxy(t)
	p.SetRulesBySource(map[string][]EgressRule{"": {{Access: "allow", Host: "a.com"}}})
	p.SetDenyWaitBySource(map[string]time.Duration{"": time.Minute})

	r := p.awaitEgress(context.Background(), ac(p, "", "GET", "a.com", "/"), 443, "no matching rule", 403)
	if r == nil || r.Access != "allow" {
		t.Fatalf("awaitEgress = %v, want allow rule", r)
	}
}

// TestAwaitEgressNoWait: with no deny-wait configured, a denied request returns
// nil immediately and emits the full request+response deny pair (historical
// behavior).
func TestAwaitEgressNoWait(t *testing.T) {
	p, buf := auditProxy(t)
	p.SetRulesBySource(map[string][]EgressRule{"": {{Access: "allow", Host: "a.com"}}})

	start := time.Now()
	if r := p.awaitEgress(context.Background(), ac(p, "", "GET", "denied.com", "/"), 443, "no matching rule", 403); r != nil {
		t.Fatalf("awaitEgress = %v, want nil (denied, no wait)", r)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("denied-with-no-wait took %v, want ~immediate", elapsed)
	}
	out := buf.String()
	if !strings.Contains(out, `"phase":"request"`) || !strings.Contains(out, `"phase":"response"`) {
		t.Fatalf("no-wait deny should emit request+response events, got: %s", out)
	}
}

// TestAwaitEgressEmitsDenyImmediately is the regression guard for the bug this
// feature must avoid: with a grace period configured, the phase:"request" deny
// event MUST be emitted up front (so a client can widen the policy), NOT after
// the wait elapses.
func TestAwaitEgressEmitsDenyImmediately(t *testing.T) {
	p, buf := auditProxy(t)
	p.SetRulesBySource(map[string][]EgressRule{"": {}})
	p.SetDenyWaitBySource(map[string]time.Duration{"": 10 * time.Second})

	done := make(chan *EgressRule, 1)
	go func() {
		done <- p.awaitEgress(context.Background(), ac(p, "", "GET", "held.com", "/"), 443, "no matching rule", 403)
	}()

	// The deny request event must appear well before the 10s grace period.
	deadline := time.After(2 * time.Second)
	for {
		out := buf.String()
		if strings.Contains(out, `"host":"held.com"`) &&
			strings.Contains(out, `"phase":"request"`) &&
			strings.Contains(out, `"verdict":"deny"`) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("deny request event not emitted during the wait; audit=%q", out)
		case <-time.After(10 * time.Millisecond):
		}
	}
	// No response (final) deny should have fired yet — the request is still held.
	if strings.Contains(buf.String(), `"phase":"response"`) {
		t.Fatalf("response deny emitted before the grace period elapsed: %s", buf.String())
	}

	// Release it by widening the policy; it should now proceed and never emit a
	// response-deny.
	p.SetRulesBySource(map[string][]EgressRule{"": {{Access: "allow", Host: "held.com"}}})
	select {
	case r := <-done:
		if r == nil || r.Access != "allow" {
			t.Fatalf("awaitEgress = %v, want allow after grant", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitEgress did not release after the grant")
	}
	if strings.Contains(buf.String(), `"phase":"response"`) {
		t.Fatalf("response deny emitted for a request that was ultimately allowed: %s", buf.String())
	}
}

// TestAwaitEgressReleasedByPolicyUpdate: a denied request paused under a
// deny-wait is released the moment a policy update allows it — well before the
// grace period would elapse.
func TestAwaitEgressReleasedByPolicyUpdate(t *testing.T) {
	p, _ := auditProxy(t)
	p.SetRulesBySource(map[string][]EgressRule{"": {}})
	p.SetDenyWaitBySource(map[string]time.Duration{"": 10 * time.Second})

	got := make(chan *EgressRule, 1)
	go func() {
		got <- p.awaitEgress(context.Background(), ac(p, "", "GET", "late.com", "/"), 443, "no matching rule", 403)
	}()

	// Give the waiter time to park on the deny-wait, then widen the policy.
	time.Sleep(50 * time.Millisecond)
	p.SetRulesBySource(map[string][]EgressRule{"": {{Access: "allow", Host: "late.com"}}})

	select {
	case r := <-got:
		if r == nil || r.Access != "allow" || r.Host != "late.com" {
			t.Fatalf("awaitEgress = %v, want allow rule for late.com", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitEgress did not release after the policy update")
	}
}

// TestAwaitEgressDeadline: if no update arrives, the paused request is denied
// (nil) once the grace period elapses, and the response-deny finally fires.
func TestAwaitEgressDeadline(t *testing.T) {
	p, buf := auditProxy(t)
	p.SetRulesBySource(map[string][]EgressRule{"": {}})
	p.SetDenyWaitBySource(map[string]time.Duration{"": 80 * time.Millisecond})

	start := time.Now()
	r := p.awaitEgress(context.Background(), ac(p, "", "GET", "never.com", "/"), 443, "no matching rule", 403)
	elapsed := time.Since(start)
	if r != nil {
		t.Fatalf("awaitEgress = %v, want nil after deadline", r)
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("returned after %v, want >= deny-wait (80ms)", elapsed)
	}
	if !strings.Contains(buf.String(), `"phase":"response"`) {
		t.Fatalf("expected a response deny after the deadline, got: %s", buf.String())
	}
}

// TestAwaitEgressContextCancel: cancelling the context ends the wait early,
// returning a deny without waiting out the grace period.
func TestAwaitEgressContextCancel(t *testing.T) {
	p, _ := auditProxy(t)
	p.SetRulesBySource(map[string][]EgressRule{"": {}})
	p.SetDenyWaitBySource(map[string]time.Duration{"": 10 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan *EgressRule, 1)
	go func() { got <- p.awaitEgress(ctx, ac(p, "", "GET", "x.com", "/"), 443, "no matching rule", 403) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case r := <-got:
		if r != nil {
			t.Fatalf("awaitEgress = %v, want nil after context cancel", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitEgress did not return after context cancel")
	}
}

// TestAwaitEgressStillDeniedAfterUnrelatedUpdate: a policy update that does not
// allow the pending host wakes the waiter, which re-evaluates and keeps waiting
// until the deadline.
func TestAwaitEgressStillDeniedAfterUnrelatedUpdate(t *testing.T) {
	p, _ := auditProxy(t)
	p.SetRulesBySource(map[string][]EgressRule{"": {}})
	p.SetDenyWaitBySource(map[string]time.Duration{"": 150 * time.Millisecond})

	got := make(chan *EgressRule, 1)
	go func() {
		got <- p.awaitEgress(context.Background(), ac(p, "", "GET", "want.com", "/"), 443, "no matching rule", 403)
	}()

	// An update that allows a different host must not release this waiter.
	time.Sleep(30 * time.Millisecond)
	p.SetRulesBySource(map[string][]EgressRule{"": {{Access: "allow", Host: "other.com"}}})

	select {
	case r := <-got:
		if r != nil {
			t.Fatalf("awaitEgress = %v, want nil (unrelated update, then deadline)", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitEgress never returned")
	}
}
