# Design: Event Telemetry Pipeline (OTel → Kafka/Pub/Sub → BigQuery)

Status: Proposal
Author: Emmanuel Garcia
Related: builds on `multi-sandbox-pod.md` (per-sandbox brokers in a pack pod).

## 1. Motivation

Every sandbox emits a rich stream of structured events — `system`, `stdio`,
`exec.*`, `egress.*`, `fs.*`, `ingress.*`, `config.apply`, `resource.usage` —
through a per-sandbox in-memory broker
([internal/events/broker.go](../../internal/events/broker.go)). Today these
events have exactly two consumers:

1. **`GET /v1/events`** — a live SSE stream per sandbox
   ([handlers/events.go](../../internal/api/handlers/events.go)). Nothing is
   retained past the broker's 500-entry ring
   ([DefaultCapacity](../../internal/events/broker.go)).
2. **The inspector** — holds an SSE connection per sandbox and persists what it
   sees into a local SQLite DB (`~/.hiver/events.db`,
   [eventStore.ts](../../cli/packages/inspector-server/src/lib/eventStore.ts)),
   pruned after 24h.

Two gaps follow:

- **No durable, queryable history.** Once a sandbox is gone and the inspector's
  local TTL lapses, its event history is unrecoverable. There is no fleet-wide
  analytical view (egress patterns, exec volume, failure rates across images).
- **The controller only sees lifecycle.** The fan-in in
  [k8s_runtime.go](../../internal/api/controller/k8s_runtime.go) `eventsPacked`
  collapses each pod's stream down to `SandboxLifecycleEvent`. The high-value
  detail events never leave the node they were produced on.

This proposes a durable collection pipeline: tap every event at the broker,
export it over OpenTelemetry, and land it in a data warehouse (BigQuery), with a
read path so the inspector can render a sandbox's full history **by key** even
after the sandbox and the local cache are gone.

### 1.1 Developer-facing contract

The plumbing below exists to deliver a small contract to a developer building on
hiver. Everything after this section is *how*; this is *what they get*:

- **Collection is automatic for what matters.** A developer instruments nothing.
  A sandbox is collected if it **errors** (always — failed runs are never lost),
  is **opted in**, or is **sampled** (§6.1). A collected sandbox keeps its
  *complete* event stream, never a thinned subset.
- **Opt-in is one config field.** `telemetry.collect: true` on `SandboxConfig`
  forces full collection for a run — a debugging session, a canary (§6.1, path 2).
- **Lookup is by key and outlives the sandbox.** `hiver events <key>` and the
  inspector's key search return a sandbox's full history from the warehouse *after
  it's gone*, scoped by `--start`/`--end` or date range (§8).
- **Reading uses the developer's own access, not a service.** The CLI and
  inspector query locally via `~/.hiver/config.json` (`eventStore`) plus ambient
  cloud credentials (§8.1). No deployed endpoint; absent config or creds, lookups
  fall back to the live stream + local cache rather than erroring.
- **What's in the record.** Console output (`stdio`) is kept in full; third-party
  `egress` request/response bodies are stripped (§6). So a history has the
  sandbox's own logs, but not the bodies of the external calls it made.

Two surfaces this contract introduces are **net-new and don't exist yet** (called
out so they aren't mistaken for current API): the `telemetry.collect` field on
`SandboxConfig`, and the `eventStore` block in `~/.hiver/config.json`. The
developer-facing write-up of this contract lives in the public docs; this section
is its internal source of truth.

## 2. Goals / Non-goals

**Goals**

- Collect **all** event types, not just lifecycle, with no change to the
  live-SSE behavior clients already depend on.
- Keep the publish hot path non-blocking: a stalled or slow exporter must never
  stall `Broker.Publish`.
- Transport-agnostic egress: sandboxd speaks only OTLP; Kafka vs Pub/Sub is a
  Collector config choice, not a code choice.
- Redact secret-bearing payloads **before they leave the node**: strip
  `egress.*` bodies, secret-scan `stdio` (which is itself the log, so it's kept).
- Give the inspector **and the `hiver events <sandbox-key>` CLI** a **sandbox
  key → logs** lookup backed by the warehouse — including for sandboxes that are
  already gone.

**Non-goals**

- Replacing the live SSE stream or the inspector's local SQLite cache. Both stay
  for the interactive, low-latency path; the warehouse is the durable/history
  path.
- Distributed tracing / span correlation across sandboxes (possible later; see
  §12). These are discrete events, modeled as OTel **Logs**, not spans.
- Exactly-once delivery. The pipeline is at-least-once; dedup is a query-time
  concern (§7).

## 3. Architecture overview

```
 sandboxd (per node, one broker per sandbox)          OTel Collector            Warehouse
┌─────────────────────────────────────────────┐        ┌──────────────────┐
│  broker.Publish ─┬─► SSE subscribers        │ OTLP   │ otlp receiver    │  kafka   ┌──────────┐
│                  │                          │ /gRPC  │ batch            │  ──or──► │ BigQuery │
│                  └─► otelSink.Emit ─────────┼───────►│ (redact already  │  gcp-    └──────────┘
│                      (redact; per-key,      │  :4317 │  done in-proc)   │  pubsub
│                       non-blocking buffer)  │        │ kafka/gcp export │
└─────────────────────────────────────────────┘        └──────────────────┘
                                                                                          ▲
                                                                                          │ read path
                        LOCAL (operator's machine)                                        │ (ADC)
                  ┌──────────────────────────────────┐                                    │
  web client ────►│ inspector server: query module   │────── query (key + date range) ────┘
  hiver events ──►│ + SQLite cache · ~/.hiver/config │
                  └──────────────────────────────────┘
```

The **broker is the single choke point** — every event of every type passes
through `Broker.publish` before fan-out. That is where we tap. Because the
high-volume events only ever exist in the per-sandbox broker (never forwarded to
the controller), the tap must live **in sandboxd**, one sink per broker.

## 4. In-process capture: the broker sink

### 4.1 Sink interface

Add an optional, fire-and-forget sink to the broker. It is invoked inside
`publish` after the subscriber fan-out, and must not block:

```go
// internal/events/sink.go
type Sink interface {
    Emit(Entry) // MUST NOT block; buffer + drop internally
}

func (b *Broker) SetSink(s Sink) { b.sink = s } // set once, before first Publish
```

```go
// internal/events/broker.go — inside publish(), after the subscriber loop:
if s := b.sink; s != nil {
    s.Emit(entry)
}
```

This mirrors the existing `activityHook` pattern (set-once, invoked from
`publish`) so it introduces no new locking discipline.

### 4.2 Non-blocking delivery

The OTel sink owns a bounded ring/channel and a drain goroutine. `Emit` does a
non-blocking send; on overflow it increments a dropped-events counter (itself
exported as a metric) rather than blocking the publisher. This preserves the
broker's existing "slow consumer drops, never stalls" contract — non-blocking is
a hard requirement (§2: a blocking sink would stall `Broker.Publish`).

An overflow drop is the **one** thing that can breach the per-key completeness
goal (§6.1): it punches a hole in a sampled-in key's timeline. So it's treated as
a monitored failure, not normal operation — the buffer is sized for the sandbox's
peak event rate and the drop counter is alarmed, so in steady state drops are
zero and completeness holds. (If zero-loss ever has to be guaranteed rather than
engineered-for, the fallback is spill-to-disk on the drain path; out of scope
here.)

### 4.3 Wiring

One tap point for real sandboxes: [packed.go:336](../../internal/sandboxd/packed.go#L336),
immediately after `broker := events.New(...)`:

```go
broker := events.New(events.DefaultCapacity, 0)
// When OTel is on, ALWAYS attach a sink; the admission decision (§6.1) lives
// inside it, not here — the error-tail path must observe the stream to catch a
// failure in a key that wasn't baseline-sampled. The sink ships immediately for
// baseline/opt-in keys and otherwise buffers a bounded ring, shipping only if an
// error triggers. `collect` carries the opt-in (config flag / allowlist).
if s.otel != nil {
    broker.SetSink(s.otel.SandboxSink(otel.SandboxIdentity{
        Key:     key,
        ID:      s.RoutingID().String(), // pod routing id (POD_IP-derived UUID)
        Image:   specImage(sp),
        Collect: sp.TelemetryCollect,    // path-2 opt-in
    }))
}
```

`s.otel` is built once at process start in
[cmd/sandboxd/main.go](../../cmd/sandboxd/main.go), gated on an `--otel-endpoint`
flag. **When the flag is unset the sink is nil and behavior is byte-for-byte
unchanged** — this is a no-op until you point it at a Collector.

## 5. Event identity & attributes

The inspector addresses a sandbox by the **`(id, key)`** pair carried in its
data-plane routes (`/:id/:key/...`, see
[sandboxFromReq.ts](../../cli/packages/inspector-server/src/lib/sandboxFromReq.ts)),
where `id` is the pod routing id and `key` is the sandbox key within the pod. To
make "key → logs" work in the warehouse, every exported record carries that
identity plus the dimensions searches filter on:

| Attribute            | Source                                   | Purpose                                    |
| -------------------- | ---------------------------------------- | ------------------------------------------ |
| `sandbox.key`        | `key` at `createPacked`                  | Unique per sandbox (even in pack mode)     |
| `sandbox.id`         | supervisor `RoutingID()` (POD_IP UUID)   | Pod routing id; pairs with key (see below) |
| `sandbox.image`      | `specImage(sp)`                          | Fleet analytics / filtering                |
| `event.id`           | broker `Entry.ID`                        | Per-sandbox monotonic order + resume       |
| `event.type`         | `SandboxEvent.Discriminator()`           | e.g. `egress.request`                      |
| `k8s.pod.name`,      | Downward API env                         | OTel **resource** attrs, set once/process  |
| `k8s.node.name`, …   |                                          |                                            |

The **`(id, key)` pair is the unique per-lifetime sandbox identity** — the same
identity the rest of the system already keys on. `key` is unique per sandbox even
in pack mode; `id` (the pod routing id, shared across a pack pod) disambiguates a
key that recurs across lifetimes, because a reused key lands on a different pod
and so carries a different `id`. This mirrors the inspector's own `${id}:${key}`
partition, whose comment states the invariant: *"key-reuse across lifetimes
(different id) never collides."* No synthetic `run_id` is needed — `(id, key)`
already separates lifetimes, and the dedup key (§7.2) is exactly this pair plus
`event.id`.

> Edge case: a key deleted and recreated **on the same still-live pack pod**
> reuses the same `id`, so `(id, key)` repeats. This is a pre-existing limitation
> the current inspector already carries (its `${id}:${key}` partition collides the
> same way); the time-range search (§8.1) narrows a query to one lifetime when it
> matters. If that edge ever needs to be closed, mint a per-instance id at
> `createPacked` — but that's out of scope here.

### 5.1 OTLP Logs mapping

Each `SandboxEvent` becomes one OTel **Log Record**:

- `Timestamp` ← event timestamp
- `EventName` ← discriminator (`egress.request`, `stdio`, …)
- Attributes ← the identity table above + per-type promoted fields
- `Body` ← the redacted structured payload (see §6)

Logs (not spans) are the correct signal: these are discrete, zero-duration facts.

## 6. Redaction (in the sink, before the wire)

Per the transport/redaction decision, redaction happens **in-process**, so no
secret leaves the node. It distinguishes two kinds of payload:

- **Third-party payloads** — `egress.*` request/response bodies (the sandbox
  talking to external hosts). Stripped; the inspector shows metadata, not bodies.
- **First-party console output** — `stdio`. This *is* the log the inspector must
  render (§1, §8), so it is **kept**, only scanned for secret patterns. Stripping
  it would defeat the feature.

Redaction transforms an event's payload — it **never drops the event**. Every
event of a sampled-in sandbox is logged (sampling is per key, §6.1), so a redacted
`egress.chunk` still produces a record; only its *body* is stripped. The policy is
one testable function keyed on the discriminator:

```go
// Always returns a record; redaction only rewrites body/attrs, never omits.
func redact(disc string, raw json.RawMessage) (attrs []attr, body any) {
    switch disc {
    case "stdio":            // KEEP content (it is the log); scrub secret patterns only
    case "egress.request":   // keep method/host/status; strip headers+body (or allowlist)
    case "egress.response":  // keep status/size; strip body
    case "egress.chunk":     // keep seq + byte count; strip payload bytes
    case "resource.usage":   // keep as-is (small, structured)
    default:                 // system/exec/fs/ingress/config.apply → full body
    }
}
```

Isolating this in one function (with unit tests over recorded fixtures of each
event variant) is the whole reason to redact in Go rather than in a Collector
processor: the security-sensitive logic is local, auditable, and testable, and no
secret leaves the node even for events whose metadata we keep in full.

### 6.1 Admission is per sandbox key, via three paths

Volume is controlled by admitting a **fraction of sandboxes** and logging each
admitted sandbox's stream in full — never by dropping individual events (which
would hole a key's timeline). But a purely statistical `hash(key) < rate` has a
fatal flaw for a debugging/audit tool: it is **exactly as likely to drop the
sandbox that failed as any other**. So admission is the **OR of three paths**,
evaluated by the sink:

1. **Baseline sample** — `hash(key) < sampleRate`, deterministic and stable across
   restarts. A fleet-wide statistical floor for analytics (representative, not
   targeted).
2. **Opt-in** — the sandbox config requests it (`telemetry.collect: true`) or its
   image/tenant is on an always-log allowlist. Deterministic capture of what you
   *already know* you care about: a canary image, an active debugging session, a
   specific customer.
3. **Error tail** — the sandbox emits an error-class event: non-zero agent exit,
   `system.shutdown` after a fault, failed `config.apply`, a 5xx/refused egress,
   or a crash on `stdio`. This is the path that guarantees the *interesting*
   sandbox is captured **even when it was neither sampled nor opted in**.

Baseline and opt-in are known at sink creation, so those keys **ship from the
first event**. Every other key's sink starts in **buffer mode**: it holds a
bounded pre-trigger ring (the last `tailRingEvents`, e.g. 2 000 — or the whole run
if shorter) and ships nothing. On the first error trigger it **flushes the ring
and switches to shipping** for the rest of the sandbox's life; if the sandbox ends
with no trigger, the ring is discarded and the key produces no records.

- **Sampled-in / opted-in →** complete stream, first event onward.
- **Errored →** the run-up to the error (bounded by `tailRingEvents`) plus
  everything after it. A run that errors *later* than the ring is deep loses its
  earliest events — the standard tail-sampling trade-off; size the ring to the
  debugging window you care about, or opt the image in (path 2) for full capture.
- **Otherwise →** cleanly, entirely absent.

This keeps the completeness guarantee where it matters — a key you pull up is
complete (modulo the tail-ring bound for late errors, and the never-in-steady-
state overflow drop of §4.2) — while guaranteeing **failures are never silently
dropped** and still capping volume by admitting only a fraction of the *healthy*
fleet. Cost for a not-admitted key: a bounded in-memory ring plus one cheap
discriminator check per event; it ships nothing. `sampleRate: 1.0` (log every
sandbox) is the default; lower it to cap warehouse cost — errored and opted-in
keys stay captured regardless.

## 7. Transport & warehouse (Collector-side, pluggable)

sandboxd exports OTLP only. The Collector (a per-node DaemonSet agent, optionally
fronted by a gateway) does batching, retry, and backpressure, then exports via a
**config-selected** exporter:

- **Kafka** — `kafkaexporter` → BigQuery Sink Connector (or a custom consumer).
  Prefer when Kafka already exists or multiple downstreams consume the feed.
- **Pub/Sub** — `googlecloudpubsub` exporter → a **BigQuery subscription**.

Both can run simultaneously; switching is a `exporters:` edit, no redeploy of
sandboxd.

> Landing in BigQuery is not free plumbing: a Pub/Sub BigQuery subscription
> writes the message into a table that must either match a declared schema or use
> the subscription's JSON-body column, and the Kafka BQ Sink Connector needs the
> same schema handling. The `body JSON` column (§7.1) is chosen so the payload
> lands schemalessly; the promoted columns are populated from OTLP attributes by
> the subscription/connector mapping. If that mapping proves fiddly, a thin
> transform (Dataflow, or a small consumer) is the fallback — noted so this leg
> isn't mistaken for zero-effort.

### 7.1 BigQuery schema

One wide table, partitioned by event date, clustered on the sandbox key. The
columns are the identity/filter attributes from §5 promoted out of `body`; the
redacted payload stays in the `body` JSON:

```
timestamp    TIMESTAMP,
event_id     INT64,
sandbox_key  STRING,
sandbox_id   STRING,
image        STRING,     -- fleet analytics (§1: failure rates across images)
event_type   STRING,
body         JSON        -- redacted structured payload
-- PARTITION BY DATE(timestamp)
-- CLUSTER BY sandbox_key
```

Partitioning by `DATE(timestamp)` (event time) is what makes the start/end-date
search (§8.1) prune to just the relevant days; clustering on `sandbox_key` keeps
the key lookup within those days a small scan despite the table being fleet-wide.

### 7.2 Delivery semantics

At-least-once. Duplicates are possible (Collector retry, consumer replay). The
`(sandbox_key, sandbox_id, event_id)` triple is a natural dedup key at query
time; the read path (§8) de-dupes on it when merging.

## 8. Inspector & CLI: sandbox key → logs (history)

Two consumers resolve a sandbox **by key** today, and both are limited to the
live/local view:

- **The inspector** serves a sandbox's timeline from local SQLite, keyed by
  `(owner_id, owner_key)` and filled **live** from SSE
  ([eventStore.ts](../../cli/packages/inspector-server/src/lib/eventStore.ts),
  [routes/events.ts](../../cli/packages/inspector-server/src/routes/events.ts)).
- **`hiver events <sandbox-key>`** resolves the key via `listSandboxes` (which
  returns **only live** sandboxes — it errors `no sandbox with key …` otherwise),
  replays from that same local SQLite, then attaches the live SSE stream
  ([commands/src/events/index.ts](../../cli/packages/commands/src/events/index.ts)).

Both therefore only cover sandboxes streamed while a local consumer was
connected, within the 24h TTL, and `hiver events` additionally can't address a
sandbox that has already gone. The warehouse fills both gaps.

### 8.1 Read path: a local query module in the inspector server

The read path is **not a deployed service** — it's a query module that runs
**locally inside the inspector server**, next to the SQLite cache the server
already owns. This works because every reader here is already local: the inspector
server runs on the operator's machine (it owns `~/.hiver/events.db`), it serves
the web client, and the `hiver events` CLI runs on that same machine.

So the module (`inspector-server/src/lib/eventsQuery.ts`) queries the warehouse
directly using the **operator's local BigQuery read credentials** (Application
Default Credentials — the gcloud login the operator already has) and caches
results into the existing SQLite. It's wired the same way `eventStore.ts` is
today:

- **Inspector web client** → the server's existing `/events` HTTP route, now
  backed by this module. The browser holds no credentials — the local server does.
- **`hiver events` CLI** → imports the module directly, exactly as it already
  imports `eventStore` from the inspector-server build, under the same local ADC.

The query takes just a **sandbox key and a time range**:

```
eventsQuery({ sandboxKey, start?, end? })
    → [ SandboxEvent, ... ]   // (timestamp, event_id) order, de-duped
```

`start`/`end` map straight onto the table's date partition (§7.1) so a query
prunes to the relevant days and bounds a reused key to the lifetime the caller
wants; both are optional (default: full retention window). BigQuery is queried
directly; the SQLite cache (§8.2) absorbs repeat reads, and a BI-Engine
reservation is the later lever if interactive latency needs tightening.

**Where the data-store config lives.** The module reads it from
`~/.hiver/config.json` (the same local config file, alongside `events.db`) — the
warehouse backend, GCP project, dataset, and table:

```jsonc
// ~/.hiver/config.json
{ "eventStore": { "backend": "bigquery", "project": "PROJECT",
                  "dataset": "hiver", "table": "events" } }
```

No env var and no deploy: an operator points the inspector at a warehouse by
editing this file, and credentials come from ambient ADC. When the `eventStore`
block is absent the module is inert and history falls back to local SQLite only —
so this stays a no-op until an operator opts in.

**Why local, not a deployed service.** The operator running the inspector can be
granted read-only warehouse access directly (§9.2), so a credential-holding proxy
in the cluster would add a Deployment, a Service, a gateway route, and an auth
surface just to reach data the operator can already read. Only the **write** path
keeps its creds in-cluster (the Collector's Workload Identity, §9); the **read**
path is local. If ADC is absent, the module simply returns nothing and the caller
degrades to live SSE + local SQLite (no warehouse history) rather than erroring.

### 8.2 How the inspector uses it

- **Running sandbox:** unchanged — live SSE into SQLite, served locally (fast,
  real-time).
- **Gone / historical sandbox, or events past the local TTL:** the events route
  falls back to the query module (§8.1) by key + time range, seeds SQLite, and
  serves from there. The existing `(owner_id, owner_key, owner_event_id)` cursor model
  absorbs backfilled rows unchanged.
- **Merge:** when both exist, rows are unioned and de-duped on
  `(sandbox_key, sandbox_id, event_id)`, preserving the current ordered-feed
  contract.

### 8.3 How `hiver events <sandbox-key>` uses it

The CLI must serve history by key **whether or not the sandbox is still live** —
that's the point of the durable pipeline. Two changes:

- **Resolution.** Stop making a live `listSandboxes` hit fatal. Resolve `<key>`
  through the query module (§8.1); if the sandbox happens to be live, also learn
  its `id` so the live SSE tail can be attached. A gone sandbox resolves purely
  from the warehouse instead of erroring `no sandbox with key …`. Optional
  `--start`/`--end` flags map onto the module's time range.
- **Replay source.** The existing replay-then-follow logic
  ([commands/src/events/index.ts](../../cli/packages/commands/src/events/index.ts))
  keeps local SQLite as a fast first source, then **backfills from the query
  module** for anything older than the local window (or when SQLite is empty). Live
  follow attaches only when the sandbox is still running; for a gone sandbox the
  command prints the full history and exits (nothing to follow). `--start-event-id`
  remains the explicit lower bound, now applied against the merged history.

The command already de-dupes replay against the live tail by `event.id`; the same
`(sandbox_key, sandbox_id, event_id)` key (§7.2) de-dupes warehouse rows against
local ones, and a caller who wants only one lifetime of a reused key narrows it
with `--start`/`--end`.

Because both the inspector and the CLI import the **same** query module (§8.1),
there is one query + schema surface, not two. Both run under the operator's local
ADC (the CLI directly, the web client via the local server); without ADC each
degrades to live SSE + local SQLite rather than erroring.

### 8.4 Inspector client: search by sandbox key

The inspector web client only lets you open a sandbox that's in the sidebar
list, which is the **live** set: it's fetched from `/api/sandboxes` and kept in
sync by the lifecycle stream
([App.tsx](../../cli/packages/inspector-client/src/App.tsx),
[SandboxList.tsx](../../cli/packages/inspector-client/src/components/SandboxList.tsx)).
Selection navigates to `/sandboxes/:id/:key`, and the detail view resolves the
sandbox with `sandboxes.find(s => s.key === key)` — so a key that isn't in the
live list can't be opened at all. To reach warehouse history, add a **search by
key** entry point:

- **Search input in the sidebar.** A key field (plus optional **start / end
  date** pickers) above the list. The key filters the live list as you type and,
  on submit, is treated as a sandbox key to open directly — including when it
  matches nothing live. The date range, when set, scopes the history query;
  left empty it defaults to the full retention window.
- **Resolve unknown keys via the server.** On submit with no live match, the
  client asks the inspector server (whose `/events` route is backed by the query
  module, §8.1) to fetch that key over the chosen range. The sandbox's `id` and
  status come from the returned events themselves, so no separate identity lookup
  is needed. An empty result means no events for that key in range — surfaced as
  "nothing found", distinct from an error.
- **Tolerate a not-live selection.** The detail view must stop assuming the
  selected key is in the live `sandboxes` list. When it isn't, render from the
  resolved history identity with a distinct **"archived"** status (no live dot,
  no live tail — §8.2's gone-sandbox path), so a stopped sandbox opened by key
  looks correct rather than empty.
- **Deep-linkable.** Because the route already carries `(:id, :key)`, a resolved
  historical sandbox yields a shareable URL; opening that URL re-resolves through
  the same server path, so links to gone sandboxes keep working.

This is purely a client + inspector-server change; it reuses the same local query
module as §8.2/§8.3, so no new deployed surface is introduced.

### 8.5 Nested/linked sandboxes

The broker sink only knows its **own** sandbox — the owner/nested relay model
(`nested_id`/`nested_key` in the store,
[relayLinkedSandboxEvents.ts](../../cli/packages/inspector-server/src/lib/relayLinkedSandboxEvents.ts))
is an inspector-side construct built from egress-link events. The warehouse
therefore stores **flat per-sandbox streams**. History reconstruction reuses the
same linking logic: fetch the owner's stream, discover linked sandbox keys from
its egress-link events, and fetch each linked stream by key through the same
query module (§8.1).

The reconstruction is O(links) module calls, one per linked sandbox. That's fine
for the shallow trees the inspector shows, and the calls fan out in parallel; it
degrades for deep/wide link graphs. Two bounds keep it honest: the module caps
link depth (and total linked fetches) per request, and — if deep trees become
common — an **edge table** (`owner_key, linked_key`, populated from the same
egress-link events at ingest) collapses discovery into one query instead of
re-deriving links per level. The edge table is an optimization, not required for
correctness.

## 9. Kubernetes configuration

The k8s surface is only the **write path**: producer wiring on sandbox pods, the
node-local **Collector**, and warehouse IAM. **The read path deploys nothing**
(§8.1) — it runs locally in the inspector server / CLI with the operator's own
credentials — so there is no Query service, no Service, and no gateway route.

Credential split:

- **Write** (in-cluster): the Collector holds warehouse write creds via Workload
  Identity (`pubsub.publisher`, or Kafka ACLs). sandboxd itself holds none — it
  only speaks OTLP to a node-local Collector.
- **Read** (local): the operator running the inspector holds warehouse read creds
  as ambient ADC; config comes from `~/.hiver/config.json` (§8.1). A plain IAM
  grant, not a deployment.

### 9.1 Producer side (sandboxd → Collector)

Add OTLP env to the per-image pods in
[image-pools.yaml](../../deployment/k8s/chart/templates/image-pools.yaml),
gated on `otel.enabled` so it stays a no-op by default (§4.3):

```yaml
{{- if $.Values.otel.enabled }}
- name: HOST_IP
  valueFrom: { fieldRef: { fieldPath: status.hostIP } }
- name: NODE_NAME
  valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
# sandboxd reads this as --otel-endpoint; node-local so OTLP never leaves the node
# before batching.
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: "http://$(HOST_IP):4317"
- name: OTEL_RESOURCE_ATTRIBUTES
  value: "service.name=sandboxd,k8s.node.name=$(NODE_NAME),k8s.pod.name=$(POD_NAME)"
{{- end }}
```

The Collector runs as a **DaemonSet** (one per node, `hostPort: 4317`) with a
ConfigMap pipeline: `otlp` receiver → `batch` → the selected exporter. Redaction
is already done in-process (§6), so the Collector just batches and ships. GCP auth
for the `googlecloudpubsub` exporter comes from a **Workload Identity** service
account (`pubsub.publisher`), never a mounted key.

### 9.2 Warehouse IAM

- **Collector SA** — `pubsub.publisher` on the topic (or Kafka produce ACLs). The
  only write credential, in-cluster.
- **Operator read grant** — `roles/bigquery.dataViewer` + `roles/bigquery.jobUser`
  on the dataset, granted to whoever runs the inspector. Not a workload identity,
  not a deployment — a person's (or their laptop's) ADC. Revocable per-operator.

### 9.3 values.yaml surface (write path only)

```yaml
otel:
  enabled: false                      # default off → sink nil, no behavior change
  # Per-KEY admission (§6.1), OR of three paths. Never per-event thinning.
  admission:
    # Path 1 — baseline statistical floor: fraction of the HEALTHY fleet logged
    # in full. 1.0 = every sandbox. Lowering it drops whole keys, never events.
    sampleRate: 1.0
    # Path 2 — always log these images/tenants regardless of sampleRate (canaries,
    # debugging). Sandboxes can also opt in per-run via config `telemetry.collect`.
    alwaysLogImages: []
    # Path 3 — error tail: log any sandbox that hits an error-class event, even if
    # unsampled. tailRingEvents bounds the pre-error history retained before the
    # trigger flushes and streaming begins.
    errorTail: true
    tailRingEvents: 2000
  collector:
    image: otel/opentelemetry-collector-contrib:latest
    exporter: googlecloudpubsub       # or "kafka"
    pubsubTopic: projects/PROJECT/topics/hiver-events
    kafkaBrokers: []
    serviceAccount: otel-collector@PROJECT.iam.gserviceaccount.com
```

The read side has **no Helm surface** — its config is the local
`~/.hiver/config.json` `eventStore` block (§8.1), not cluster values.

## 10. Rollout

1. **In-process, no infra (this repo).** `events/sink.go`, `internal/telemetry`
   (OTLP log exporter + `otelSink` + `redact` with tests), wire at
   [packed.go:336](../../internal/sandboxd/packed.go#L336) behind
   `--otel-endpoint` (default off → no-op).
2. **Collector + warehouse (cluster config, §9).** DaemonSet Collector, `otlp`
   receiver, `batch`, Kafka **and** Pub/Sub exporters (one enabled), BQ table
   DDL + subscription/connector. Shipped as Helm chart values.
3. **Read path (local, no deploy).** The `eventsQuery` module in the inspector
   server + CLI (§8.1), reading `~/.hiver/config.json` and the operator's ADC;
   plus the inspector/CLI/client history wiring (§8). No cluster change.

Each stage is independently shippable; stage 1 is inert until a Collector
endpoint is configured.

## 11. Concerns

- **Node-local transport is not the bottleneck (rough sizing).** A pack host runs
  O(30) sandboxes; a chatty agent averages ~10 events/s (bursting to hundreds), so
  ~300 events/s/node × ~1 KB/record ≈ **0.3 MB/s of OTLP per node** — negligible
  for a node-local Collector, whose `batch` processor sustains 10⁴–10⁵ records/s
  per core. OTLP-as-bulk-pipe was the worry; at this rate it isn't one. Bursts
  (a sandbox dumping a large log) are absorbed by the sink's bounded buffer (§4.2)
  and Collector batching. **The real cost is warehouse ingest + storage**, and
  that's what admission (§6.1) controls: at `sampleRate: 0.1` the healthy fleet is
  ~1/10th the rows, with errored + opted-in keys always kept.
- **Query latency (rough sizing).** A single-key, single-day BigQuery lookup scans
  only the matching `sandbox_key` cluster blocks — a few MB, **~1–2 s wall**
  (dominated by BQ's ~0.5–1 s query-scheduling floor), at roughly the 10 MB
  minimum-scan cost (~$0.00005). Fine for "open this sandbox"; too slow for
  type-ahead — so the sidebar search debounces, and the local query module caches
  results in the inspector's existing SQLite (a BI-Engine reservation is the later
  lever for hot keys, §8.1). The web client never hits BQ directly; it goes through
  the local server, which does.
- **Volume shape.** `stdio`, `resource.usage`, `egress.chunk` dominate per-key
  volume; `stdio` is kept in full (it's the log, §6), so per-key sampling — not
  per-event thinning — is the only volume lever that preserves timelines.
- **PII/secrets.** Addressed by in-sink redaction before egress (§6). Hard
  requirement, not a follow-up.
- **Ordering.** `event.id` is monotonic **per sandbox**, which is all the
  inspector needs; there is no global order and none is implied.
- **Backpressure.** Bounded buffer + drop-with-counter in the sink (§4.2); the
  Collector owns retry/queueing beyond the node.

## 12. Alternatives considered

- **Tap at the controller instead of the broker.** Rejected: the controller only
  sees lifecycle; the detail events never reach it. Collecting "all events"
  forces a node-local tap.
- **Kafka/Pub/Sub producer directly in sandboxd.** Rejected: puts transport
  choice, batching, and retry in the hot binary. OTLP + Collector keeps sandboxd
  thin and the sink swappable.
- **A deployed Events Query service in-cluster.** Rejected: a credential-holding
  proxy just to reach data the local operator can already read — extra Deployment,
  Service, gateway route, and auth surface. The read path runs **locally** in the
  inspector server instead (§8.1), holding the operator's own ADC. Interactive
  latency (the original reason to avoid client→BQ) is handled by the SQLite cache;
  the *web client* still never touches BQ — it goes through the local server.
- **Model events as spans (tracing).** Rejected: they are discrete facts, not
  durations; Logs is the correct OTel signal.
