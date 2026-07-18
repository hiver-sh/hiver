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

## 2. Goals / Non-goals

**Goals**

- Collect **all** event types, not just lifecycle, with no change to the
  live-SSE behavior clients already depend on.
- Keep the publish hot path non-blocking: a stalled or slow exporter must never
  stall `Broker.Publish`.
- Transport-agnostic egress: sandboxd speaks only OTLP; Kafka vs Pub/Sub is a
  Collector config choice, not a code choice.
- Redact secret-bearing payloads (`stdio`, `egress.*` bodies) **before they
  leave the node**.
- Give the inspector a **sandbox key → logs** lookup backed by the warehouse.

**Non-goals**

- Replacing the live SSE stream or the inspector's local SQLite cache. Both stay
  for the interactive, low-latency path; the warehouse is the durable/history
  path.
- Distributed tracing / span correlation across sandboxes (possible later; see
  §9). These are discrete events, modeled as OTel **Logs**, not spans.
- Exactly-once delivery. The pipeline is at-least-once; dedup is a query-time
  concern (§7).

## 3. Architecture overview

```
 sandboxd (per node, one broker per sandbox)          OTel Collector            Warehouse
┌─────────────────────────────────────────────┐        ┌──────────────────┐
│  broker.Publish ─┬─► SSE subscribers        │ OTLP   │ otlp receiver    │  kafka   ┌──────────┐
│                  │                          │ /gRPC  │ batch            │  ──or──► │ BigQuery │
│                  └─► otelSink.Emit ─────────┼───────►│ (redact already  │  gcp-    └──────────┘
│                      (redact + sample,      │  :4317 │  done in-proc)   │  pubsub
│                       non-blocking buffer)  │        │ kafka/gcp export │
└─────────────────────────────────────────────┘        └──────────────────┘
                                                                                          ▲
                                                                                          │ read path
                                                      ┌──────────────────┐  query   ┌─────┴──────┐
                            inspector server ────────► Events Query API  ├─────────►│ serving /  │
                            (key → logs, history)     └──────────────────┘          │ BigQuery   │
                                                                                    └────────────┘
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
broker's existing "slow consumer drops, never stalls" contract.

### 4.3 Wiring

One tap point for real sandboxes: [packed.go:336](../../internal/sandboxd/packed.go#L336),
immediately after `broker := events.New(...)`:

```go
broker := events.New(events.DefaultCapacity, 0)
if s.otel != nil {
    broker.SetSink(s.otel.SandboxSink(otel.SandboxIdentity{
        Key:   key,
        ID:    s.RoutingID().String(), // pod routing id (POD_IP-derived UUID)
        Image: specImage(sp),
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
identity plus enough to disambiguate reuse over time:

| Attribute            | Source                                   | Purpose                                    |
| -------------------- | ---------------------------------------- | ------------------------------------------ |
| `sandbox.key`        | `key` at `createPacked`                  | Primary lookup dimension                   |
| `sandbox.id`         | supervisor `RoutingID()` (POD_IP UUID)   | Disambiguates key across pods              |
| `sandbox.run_id`     | new per-`createPacked` UUID              | Disambiguates a **reused key over time**   |
| `sandbox.image`      | `specImage(sp)`                          | Fleet analytics / filtering                |
| `event.id`           | broker `Entry.ID`                        | Per-sandbox monotonic order + resume       |
| `event.type`         | `SandboxEvent.Discriminator()`           | e.g. `egress.request`                      |
| `k8s.pod.name`,      | Downward API env                         | OTel **resource** attrs, set once/process  |
| `k8s.node.name`, …   |                                          |                                            |

A key can recur (a later sandbox reuses the same key on a different pod). `id`
separates concurrent placements; `run_id` separates lifetimes of the same key so
the inspector never merges two runs into one timeline.

### 5.1 OTLP Logs mapping

Each `SandboxEvent` becomes one OTel **Log Record**:

- `Timestamp` ← event timestamp
- `EventName` ← discriminator (`egress.request`, `stdio`, …)
- Attributes ← the identity table above + per-type promoted fields
- `Body` ← the redacted structured payload (see §6)

Logs (not spans) are the correct signal: these are discrete, zero-duration facts.

## 6. Redaction (in the sink, before the wire)

Per the transport/redaction decision, redaction happens **in-process**, so raw
`stdio`/`egress` bodies never cross the network. The policy is one testable
function keyed on the discriminator:

```go
func redact(disc string, raw json.RawMessage) (attrs []attr, body any, keep bool) {
    switch disc {
    case "stdio":            // keep stream + byte count; hash or drop body
    case "egress.request":   // keep method/host/status; strip headers+body (or allowlist)
    case "egress.response":  // keep status/size; strip body
    case "egress.chunk":     // drop entirely (highest volume, lowest value)
    case "resource.usage":   // sample: emit 1/N or on threshold change only
    default:                 // system/exec/fs/ingress/config.apply → full body
    }
}
```

Isolating this in one function (with unit tests over recorded fixtures of each
event variant) is the whole reason to redact in Go rather than in a Collector
processor: the security-sensitive logic is local, auditable, and testable, and
sampling drops volume before it ever costs network or warehouse ingest.

## 7. Transport & warehouse (Collector-side, pluggable)

sandboxd exports OTLP only. The Collector (a per-node DaemonSet agent, optionally
fronted by a gateway) does batching, retry, and backpressure, then exports via a
**config-selected** exporter:

- **Kafka** — `kafkaexporter` → BigQuery Sink Connector (or a custom consumer).
  Prefer when Kafka already exists or multiple downstreams consume the feed.
- **Pub/Sub** — `googlecloudpubsub` exporter → a **BigQuery subscription**
  (direct writes, no Dataflow) or Dataflow for transforms.

Both can run simultaneously; switching is a `exporters:` edit, no redeploy of
sandboxd.

### 7.1 BigQuery schema

One wide table, partitioned by ingest day, clustered on the lookup dimensions:

```
timestamp    TIMESTAMP,
event_id     INT64,
sandbox_key  STRING,
sandbox_id   STRING,
run_id       STRING,
image        STRING,
event_type   STRING,
body         JSON        -- redacted structured payload
-- PARTITION BY DATE(timestamp)
-- CLUSTER BY sandbox_key, sandbox_id, event_type
```

Clustering on `sandbox_key` is what keeps the inspector's key→logs lookups
cheap (small scan) despite the table being fleet-wide.

### 7.2 Delivery semantics

At-least-once. Duplicates are possible (Collector retry, consumer replay). The
`(sandbox_id, run_id, event_id)` triple is a natural dedup key at query time; the
inspector read path (§8) de-dupes on it when merging.

## 8. Inspector: sandbox key → logs (history)

Today the inspector serves a sandbox's timeline from local SQLite, keyed by
`(owner_id, owner_key)` and filled **live** from SSE
([eventStore.ts](../../cli/packages/inspector-server/src/lib/eventStore.ts),
[routes/events.ts](../../cli/packages/inspector-server/src/routes/events.ts)).
That only covers sandboxes streamed while the inspector was connected, and only
within the 24h TTL. The warehouse fills the gap.

### 8.1 Read path: an Events Query API (not inspector → BigQuery directly)

The inspector should **not** query BigQuery directly:

- Interactive BQ latency (~1–3s/query) and per-scan cost are wrong for an
  interactive UI.
- It would couple the inspector (and every CLI user) to GCP credentials and the
  warehouse's physical schema.

Instead, introduce a small **Events Query API** — a read endpoint keyed by
sandbox identity — that the inspector calls:

```
GET /events?sandbox_key=<key>&sandbox_id=<id>&run_id=<opt>&after=<event_id>
    → [ SandboxEvent, ... ]   # ordered by event_id, de-duped
```

It can live in the controller (already the fleet's read plane) or as a dedicated
service. Its backing store is swappable behind the API: BigQuery direct (with the
clustering above) to start, a cache/BI-Engine or a dedicated serving DB later —
without touching the inspector.

### 8.2 How the inspector uses it

- **Running sandbox:** unchanged — live SSE into SQLite, served locally (fast,
  real-time).
- **Gone / historical sandbox, or events past the local TTL:** the events route
  falls back to the Query API by `(id, key)`, seeds SQLite, and serves from
  there. The existing `(owner_id, owner_key, owner_event_id)` cursor model
  absorbs backfilled rows unchanged.
- **Merge:** when both exist, rows are unioned and de-duped on
  `(sandbox_id, run_id, event_id)`, preserving the current ordered-feed contract.

### 8.3 Nested/linked sandboxes

The broker sink only knows its **own** sandbox — the owner/nested relay model
(`nested_id`/`nested_key` in the store,
[relayLinkedSandboxEvents.ts](../../cli/packages/inspector-server/src/lib/relayLinkedSandboxEvents.ts))
is an inspector-side construct built from egress-link events. The warehouse
therefore stores **flat per-sandbox streams**. History reconstruction reuses the
same linking logic: fetch the owner's stream, discover linked sandbox keys from
its egress-link events, and fetch each linked stream by key through the same
Query API. No new edge table is required (though one could be added later to skip
re-deriving links).

## 9. Rollout

1. **In-process, no infra (this repo).** `events/sink.go`, `internal/telemetry`
   (OTLP log exporter + `otelSink` + `redact` with tests), wire at
   [packed.go:336](../../internal/sandboxd/packed.go#L336) behind
   `--otel-endpoint` (default off → no-op).
2. **Collector + warehouse (cluster config).** DaemonSet Collector, `otlp`
   receiver, `batch`, Kafka **and** Pub/Sub exporters (one enabled), BQ table
   DDL + subscription/connector. Shipped as `deployment/` examples.
3. **Read path.** Events Query API + inspector history fallback (§8).

Each stage is independently shippable; stage 1 is inert until a Collector
endpoint is configured.

## 10. Concerns

- **Volume/cost.** `stdio`, `resource.usage`, `egress.chunk` dominate. Sampling
  and dropping in the sink (§6) is the primary control; clustering (§7.1) bounds
  query cost.
- **PII/secrets.** Addressed by in-sink redaction before egress (§6). This is a
  hard requirement, not a follow-up.
- **Ordering.** `event.id` is monotonic **per sandbox**, which is all the
  inspector needs; there is no global order and none is implied.
- **Backpressure.** Bounded buffer + drop-with-counter in the sink (§4.2); the
  Collector owns retry/queueing beyond the node.

## 11. Alternatives considered

- **Tap at the controller instead of the broker.** Rejected: the controller only
  sees lifecycle; the detail events never reach it. Collecting "all events"
  forces a node-local tap.
- **Kafka/Pub/Sub producer directly in sandboxd.** Rejected: puts transport
  choice, batching, and retry in the hot binary. OTLP + Collector keeps sandboxd
  thin and the sink swappable.
- **Inspector queries BigQuery directly.** Rejected for interactive latency,
  per-query cost, and coupling every client to GCP creds/schema (§8.1).
- **Model events as spans (tracing).** Rejected: they are discrete facts, not
  durations; Logs is the correct OTel signal.
