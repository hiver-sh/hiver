# UFFD memory backend for snapshot resume

How microVM resume gets its guest memory, why the userfaultfd backend exists,
and what is known — and still not known — about huge pages.

Related: builds on `vm-state-snapshot.md`, which covers snapshot capture and the
resume flow this plugs into.

## Background

Resuming a microVM means giving the guest back the memory recorded in
`mem.bin`. Firecracker's `/snapshot/load` accepts two `mem_backend` types:

- **`File`** — Firecracker mmaps `mem.bin` itself. Pages arrive on demand, so
  the guest takes a fault per page as it touches its working set.
- **`Uffd`** — Firecracker registers guest memory with `userfaultfd` and hands
  the fd to a handler over a Unix socket. The handler decides when pages land.

The `Uffd` path is what `internal/uffd` implements. It exists because eager
population beats demand paging here: a resumed agent touches a large, largely
predictable working set immediately, so paying for it up front in a few
parallel `UFFDIO_COPY` calls is cheaper than thousands of individual faults.

Selection is per-pool, via `sandboxServices.<name>.memBackend` in the chart
(`FIRECRACKER_MEM_BACKEND` in the pod). `hugePages` implies `uffd` on its own —
Firecracker cannot map a huge-page-backed snapshot through the `File` backend.

## Measured results

Real turns through the app, six per configuration, sandbox config verified
inside the pod each time:

| configuration | result |
| --- | --- |
| `File` backend | 6/6 |
| `uffd` alone | 6/6, twice |
| `uffd` + `hugePages`, pre-fix | intermittent failure — see below |
| `uffd` + `hugePages`, all fixes (2026-07-21) | 6/6, twice |

Against the `File` backend, `uffd` moved `POST → init` from ~880ms to ~300ms
and cut `cache_creation` from 8,695 to 2,973 tokens. With the resume fixes
below, `hugePages: "2M"` is green as well and is what `work/deployment`
now runs.

## Huge pages: resolved (2026-07-21)

The intermittent turn failure under `hugePages: "2M"` is fixed and verified on
real turns. The root cause was never memory at all: it was the **9p workspace
transport losing an in-flight request across the resume**, wedging the guest
process that issued it. Details, evidence, and the fix below; the corrected
history of the investigation's hypotheses is further down.

Pass rates on real turns with `hugePages: "2M"`:

| `hugepages-2Mi` limit | result |
| --- | --- |
| 512Mi (chart default, one VM's worth) | 1/5 |
| 2Gi (four VMs' worth) | 5/6 |
| 2Gi, repeated | 1/6 |

The 2Gi run looked like a fix and did not survive replication. The hypothesis
it was testing — that the pod's hugepages allocation is sized for a single VM
while a pool runs several concurrently, and teardown lags the DELETE so run N+1
starts while run N still holds the whole allocation — is therefore **dead as an
explanation**, even though the undersized default is real (see Known gaps).

### What the failure looked like

Identical in every failing run:

- VM resumes in ~100ms and reports ready
- `POST /conversation` returns **200 in ~3ms** — the guest is serving
- the agent then **never emits its `init` event**; no egress request follows
- guest healthy, vCPU parked in `kvm_vcpu_block`, no ENOMEM, no allocation error
- the SSE stays open; only `resource.usage` heartbeats arrive

### The evidence that cracked it

What the earlier rounds never did was look INSIDE a wedged guest. Captured live
from two independent wedged runs (via `scripts/diagnose-wedged-guest.sh` piped
through the exec channel), identical both times: every process healthy and idle
— except the resident `claude` child's main thread:

```
== pid 247 (claude)
  tid 247 state=D wchan=p9_client_rpc
    [<0>] p9_client_rpc+0xf5/0x340
    [<0>] p9_client_walk+0x81/0x220
    [<0>] v9fs_vfs_lookup.part.0+0x64/0x1c0
    ...
    [<0>] __x64_sys_statx+0x62/0x80
```

The turn arrived, `claude` woke and started processing, then blocked forever in
a `statx()` on the 9p-mounted `/workspace` — an RPC whose reply never comes.
Memory was fine. The vCPU idled because the guest genuinely had nothing to run.

### Root cause: the 9p transport swallows a request across resume

A resume kills the guest's host-facing 9p TCP connection (new netns, old
listener gone). nineproxy (in sbxguest) survives this by replaying the session
onto the re-bound listener. But there is a window after the VM resumes and
before the dead connection's RST arrives in which a guest 9p request — from
claude's file watchers, or the turn itself — is written into the doomed socket.
**That TCP write succeeds locally and the bytes evaporate.** The proxy believed
delivered-means-done, kernel v9fs has no client-side timeout, and the requester
waits forever on its tag: `p9_client_rpc`, D state, one process wedged, guest
otherwise healthy. Exactly the signature.

Why it correlated with huge pages (plausible, not proven): the loss window is
per-resume timing. With 4KiB demand paging the guest's tasks stall faulting
through that window; 2MiB population makes the guest runnable much sooner, so
the workload gets to issue 9p traffic while the connection is still dying. The
12 green 4KiB runs bound that config's loss rate; they never proved it zero —
the fix applies to every resume, not just huge-page ones.

### The fix

`nineproxy` now records the raw bytes of every unanswered request
(`Session.outstanding`, settled by any reply on the tag, honouring `Tflush`),
and `Reconnect` re-delivers them — original tags, original order — after the
session replay, while the pumps are still parked. `writeHost` became
generation-aware so a request observed before a reconnect is delivered exactly
once (by the reconnect's resend, never also by the pump's retry).
`TestProxyReconnectResendsSwallowedRequest` reproduces the swallow end-to-end
and fails against the old proxy; re-execution on the new server is safe because
the old server died with the old connection before answering anything
outstanding.

Two more real defects were found and fixed on the way, both real, neither the
wedge:

- **`UFFDIO_WAKE` was misencoded** as `_IOWR` (0xc010aa02); the kernel defines
  `_IOR` (0x8010aa02) and dispatches on the full command, so EVERY defensive
  wake since the feature landed returned EINVAL without waking anything — the
  pool logs showed dozens of `uffd: wake ...: invalid argument` per resume.
  Fixed; `TestWakeIoctlEncoding` (run on the GKE node) fails against the old
  encoding. Consequence for the history below: hypothesis 1 was never actually
  tested.
- **KVM async page faults** let a guest task park on an APF token whose "page
  ready" can be lost across restore (firecracker#3020 is that exact signature).
  Guests now boot with `no-kvmapf`: a missing page blocks the vCPU in the host
  until UFFDIO_COPY provides it — no guest-side parking, nothing to lose.
  Measured cost: none visible (turn latency unchanged).

### Verified

Same methodology as the original investigation: real turns through the app,
fresh sandbox per turn, `hugePages: "2M"`, `hugePagesLimit: 2Gi`, sandbox
config verified in the pod:

| build | result |
| --- | --- |
| no-kvmapf + wake fix only | 3/6 — still wedging, `p9_client_rpc` captured |
| + nineproxy re-delivery | **6/6, then 6/6** |

Warm-turn latency with all fixes: first output ~1.5–2.6s after POST, `done`
~2.5s — unchanged from the plain-uffd baseline.

### The hypothesis history, corrected

The original list of "tested and dead" hypotheses, annotated with what this
round actually established:

1. `UFFDIO_WAKE` missing after EEXIST (added, no change) — **never actually
   tested**: the wake ioctl was misencoded, so the added wake EINVAL'd on every
   call. The "no change" result measured a no-op. The encoding is now fixed;
   whether a working wake would have mattered for the old wedge is moot (the
   wedge was 9p, not uffd), but the logs that would have exposed this
   (`uffd: wake ...: invalid argument`) were present all along. When a fix
   "doesn't change anything", check that it executed.
2. Async workspace mount racing the guest — close cousin of the truth. The
   race was real but one layer down: not the mount RPC, the 9p transport's
   in-flight requests during the reconnect window.
3. Background population racing the guest — dead (synchronous also failed).
4. EEXIST range-abandon plus slab-start faults — a real bug, fixed, not the
   wedge.
5. Snapshot-time directory seeding — dead.
6. Turn arriving before the guest settled (2s delay, still failed) —
   consistent with the real cause: the swallowed request usually belonged to
   claude's background file watchers, which fire in the resume window
   regardless of when the turn lands; the turn merely discovered the already
   wedged process.
7. Undersized `hugepages-2Mi` allocation — dead as a cause of this failure;
   the sizing note in Known gaps still stands on its own merits.

### Related firecracker fixes worth picking up

The pinned firecracker v1.12.1 predates three restore-correctness fixes, all in
v1.16.x — none is confirmed to be this bug, but all touch the same machinery
and an upgrade is cheap insurance once it revalidates:

- [#5494](https://github.com/firecracker-microvm/firecracker/pull/5494)
  (v1.13.0): `KVM_KVMCLOCK_CTRL` before resume, fixing watchdog soft lockups on
  restored VMs.
- [#5738](https://github.com/firecracker-microvm/firecracker/pull/5738)
  (v1.16.0): snapshots now serialize the full KVM custom MSR range; v1.12.1
  omits `MSR_KVM_ASYNC_PF_ACK` (it does save `_EN` and `_INT`).
- [#5809](https://github.com/firecracker-microvm/firecracker/pull/5809)
  (v1.16.0): on host ≥5.16, `kvm-clock` guests had their monotonic clock jump
  forward on restore by the wall time elapsed since capture.

## The uffdio_copy.copy trap

The one real bug found in the handler, worth recording because it is invisible
at the call site and easy to reintroduce.

`mfill_atomic` returns `copied ? copied : err`, and the kernel writes that value
straight into `uffdio_copy.copy`. **The field is a byte count only when
positive.** On zero progress it holds a *negative errno* — an `EEXIST` that
copied nothing reports `-17`, not `0`.

Reading that as an unsigned count yields `2^64 - 17`. The original code did
exactly that, so every advance check (`skip >= size`) passed trivially and
`copyRange` returned success having abandoned the rest of the region. The guest
then silently demand-paged memory the copier believed it had already written.

`copied()` in `uffd_linux.go` is the guard; it accepts only positive values, and
its alignment check doubles as a standing assertion that a positive count under
hugetlbfs is always a 2MiB multiple.

### Testing this correctly

`TestPopulatesAroundAlreadyPresentPage` originally asserted by reading
`dst[off]` — which **faults the page in and gets served by the residual
handler**. The assertion populated the very pages it claimed to verify, and
passed against a copier that had populated nothing.

It now checks residency with `mincore` (which does not fault) and waits on the
copier's own `BytesCopied` counter. In that form it fails against the old
arithmetic, abandoning ~2MiB from the first present page onward, and passes
against the fix.

The general lesson, which cost several rounds here: **an assertion that touches
guest memory is not a test of whether guest memory was populated.**

## Known gaps

- The chart derives `hugepages-2Mi` from `resources.requests.memory`, which is
  one VM's worth, while a pool pod runs several microVMs concurrently and
  hugepages are a hard, non-reclaimable per-pod allocation. Undersized by
  design; set `hugePagesLimit` explicitly for peak concurrency. Real, but not
  the cause of the failure above.
- The `UFFDIO_WAKE` call in `serveResidual` is defensive: the race it guards
  against — a fault on an already-present page getting EEXIST and so waking
  nobody — could not be reproduced in a test (the copier's own `UFFDIO_COPY`
  wakes the range it fills, and an in-process Go reproduction is defeated by
  async-preemption signals breaking the park). Kept because a redundant wake is
  a no-op and a permanently parked guest thread is not. Note it only became a
  real wake in 2026-07 — the ioctl number was wrong from the start (see the
  huge-pages section); `TestWakeIoctlEncoding` now pins the encoding.
- `node_upgrade_max_surge` in the GKE Terraform is documented as fixing a
  reservation failure it did not fix; the actual remedy was temporarily resizing
  the reservation.

## Testing notes

Several rounds of this investigation were lost to harness bugs rather than
product bugs. If you pick this up, the traps were:

- **Sampling `ps` once** and calling a `D` state a hang. `D` on 9p is normal
  transient I/O, and `\bD\b` does not match `Dl`.
- **Grepping `'"type":"result"'`** against event bodies that are JSON-escaped
  (`\"type\":\"result\"`), so the match never fired and every run looked failed.
  Parse the JSONL.
- **Starting `hiver events` before the POST** that creates the sandbox — it
  errors with "no sandbox with key". POST first; events replay from id 1.
- **Leaking sandboxes** by skipping the DELETE. Past `--max-concurrent-launches`
  this produces firecracker slot collisions
  (`FailedToBindSocket .../N/firecracker.sock`) that look exactly like the bug
  under investigation but are entirely self-inflicted.
- **Editing values.yaml with a regex and not reading it back.** A failed
  rollback left `hugePages` set through four supposedly controlled comparisons
  and invalidated all of them. Verify `FIRECRACKER_*` and the resource limits
  *inside the pod* before trusting any result.

The `snapshots` volume is an `emptyDir`, so any pod recreation — including every
Helm upgrade that changes the pod spec — wipes it. Re-capture with
`npm run snapshot -- work` before measuring, or the first run pays a cold boot.

When a turn wedges, capture the guest BEFORE deleting the sandbox:
`scripts/diagnose-wedged-guest.sh` (piped through the exec channel, or inline
via `hiver shell <key> --command`) dumps state/wchan/kernel stack for every
agent task. One `wchan` reading is worth more than a week of host-side
hypothesis testing: `p9_client_rpc` means the 9p transport, `handle_userfault`
or `kvm_async_pf_task_wait` means memory machinery, `ep_poll` means the process
is idle and the problem is elsewhere.
