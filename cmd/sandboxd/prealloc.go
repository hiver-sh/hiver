package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"

	"github.com/hiver-sh/hiver/internal/isolation"
)

// preallocPool keeps a fixed set of preallocated sandbox slots so a create can
// claim one instead of paying the per-sandbox network setup — netns + veth + a
// batched iptables-restore (whose forks contend on the kernel-wide xtables lock,
// the dominant cost under a concurrent create burst) + the in-netns DNS sink — on
// the request path.
//
// A slot is the host-side network for one guest-IP octet (172.16.<n>.2): it is
// octet-deterministic and tenant-agnostic, so slots are *reused* rather than
// rebuilt. The pool provisions `target` slots once at startup, hands them out on
// claim, and on release resets each back to its original state (flush per-tenant
// conntrack) before returning it to `ready`. Both provisioning and resets run on a
// single worker goroutine, so the contended setup is off the request path *and*
// serialized — no concurrent xtables-lock contention from the pool itself.
//
// The pool owns its octets for the pod's lifetime; they never go back to
// packState's IP allocator. A create that finds the pool empty (all slots in use)
// falls back to allocating + wiring an octet synchronously, exactly as before.
//
// Only the network is preallocated, for both backends. The overlay is not: a
// microvm resume reopens its overlay in place from the per-key state dir and a
// cold boot builds it there on claim, while the container overlay is keyed by the
// sandbox key — both unknown until claim. The FUSE workspace is likewise
// key-coupled (host dirs live under /run/sandboxd/<key>).
type preallocPool struct {
	p      *packState
	target int

	mu    sync.Mutex
	ready []int        // octets of provisioned, unclaimed slots
	inUse map[int]bool // octets currently claimed by a sandbox

	resetCh chan int // octets to (re)provision/reset on the worker goroutine
}

func newPreallocPool(p *packState, target int) *preallocPool {
	return &preallocPool{
		p:       p,
		target:  target,
		inUse:   make(map[int]bool, target),
		resetCh: make(chan int, target),
	}
}

// start provisions the initial slots and launches the worker that resets released
// slots back into the pool. Provisioning runs on the worker too, so it is
// serialized with resets and never contends with itself.
func (pp *preallocPool) start() {
	go pp.run()
	for i := 0; i < pp.target; i++ {
		_, octet := pp.p.allocIP() // owned by the pool for the pod's lifetime; never freed
		pp.resetCh <- octet
	}
}

// run is the single worker: it provisions a slot's network the first time it sees
// an octet, then on every subsequent visit (a release) resets the slot, returning
// it to `ready` once it is clean.
func (pp *preallocPool) run() {
	provisioned := make(map[int]bool)
	for {
		var octet int
		select {
		case <-pp.p.ctx.Done():
			return
		case octet = <-pp.resetCh:
		}
		if !provisioned[octet] {
			if err := pp.provision(octet); err != nil {
				log.Printf("sandboxd: prealloc: provision octet %d: %v", octet, err)
				// Leave the octet out of the pool; a create will fall back. Don't
				// recycle it to the IP allocator — its partial state may have leaked.
				continue
			}
			provisioned[octet] = true
		} else {
			pp.reset(octet)
		}
		pp.mu.Lock()
		pp.ready = append(pp.ready, octet)
		pp.mu.Unlock()
	}
}

// provision wires a slot's packed network for the first time. The DNS sink it
// starts is bound to the pod context, so it persists across claims — it is
// tenant-agnostic (answers every lookup with the proxy placeholder). The per-VM
// overlay is intentionally not preallocated: a resume reopens it in place and a
// cold boot builds it on claim, so any overlay built here would be unused.
func (pp *preallocPool) provision(octet int) error {
	ip := slotIP(octet)
	iso, err := isolation.New(pp.p.isoKind, isolation.Config{GuestIP: ip, Hostname: pp.p.hostname})
	if err != nil {
		return err
	}
	if err := iso.RedirectEgress(pp.p.ctx, pp.p.proxyPort, pp.p.dnsPort, pp.p.soMark); err != nil {
		return fmt.Errorf("redirect egress: %w", err)
	}
	return nil
}

// reset returns a released slot to its original state without re-wiring the
// network (which is octet-deterministic and persists): it flushes per-tenant
// conntrack for the slot's source IP. The overlay needs no reset here — the next
// claim reopens it in place (resume) or builds it (cold boot). Best-effort.
func (pp *preallocPool) reset(octet int) {
	flushConntrack(pp.p.ctx, slotIP(octet))
}

// claim hands out a ready slot's octet, or ok=false when the pool is empty (the
// caller then allocates + wires an octet synchronously).
func (pp *preallocPool) claim() (ip string, octet int, ok bool) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if len(pp.ready) == 0 {
		return "", 0, false
	}
	octet = pp.ready[len(pp.ready)-1]
	pp.ready = pp.ready[:len(pp.ready)-1]
	pp.inUse[octet] = true
	return slotIP(octet), octet, true
}

// release returns a claimed slot to the pool. The worker resets it (off the
// request path) before it becomes claimable again.
func (pp *preallocPool) release(octet int) {
	pp.mu.Lock()
	delete(pp.inUse, octet)
	pp.mu.Unlock()
	pp.resetCh <- octet
}

func slotIP(octet int) string { return fmt.Sprintf("172.16.%d.2", octet) }

// flushConntrack drops conntrack entries for a reused slot's source IP so a new
// tenant on the same octet can't inherit the previous tenant's flow state. Best
// effort: the binary may be absent, and "no entries" is not an error worth
// logging.
func flushConntrack(ctx context.Context, ip string) {
	for _, dir := range []string{"-s", "-d"} {
		_ = exec.CommandContext(ctx, "conntrack", "-D", dir, ip).Run()
	}
}
