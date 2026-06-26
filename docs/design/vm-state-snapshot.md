# Design Proposal: Explicit VM-State + Files Snapshots

Status: Proposal
Author: (you)
Related: builds on `multi-sandbox-pod.md` (base snapshot / resume). Supersedes the
controller-managed `--prewarm` / `--instance-cpu` / `--instance-memory` model.

## 1. Motivation

Today "snapshot" means **one thing**: a gzip-tar of the writable filesystem
(overlay upper + local FUSE backends), captured on shutdown and restored before
launch. See [internal/snapshot/snapshot.go](../../internal/snapshot/snapshot.go),
keyed by `snapshot.restore_key` in [internal/spec/spec.go](../../internal/spec/spec.go#L69-L86).

Separately, microVMs have a *second*, richer notion of saved state — a full
firecracker VM snapshot (`snapshot.bin` + `mem.bin` + the warm overlay) — but it
is **not** exposed as "a snapshot." It is bolted onto the `--prewarm` path: the
host boots the image entrypoint, waits for `/run/hiver/prewarm-ready`, pauses,
and writes the VM snapshot ([PrewarmSnapshot](../../internal/isolation/microvm.go#L1100),
[BuildMicroVMBaseSnapshot](../../internal/isolation/microvm.go#L279)). Resume then
reconciles whatever config arrives against that frozen entrypoint over the guest
control channel ([ControlRequest](../../internal/firecracker/control.go#L19)).

Two problems:

1. **The two kinds of saved state are conflated and asymmetric.** A user can name
   a *filesystem* snapshot but cannot name, take, or restore a *VM-state*
   snapshot — that lifecycle is owned by the controller (`--prewarm`, the prealloc
   pool, base-snapshot build) and the guest sizing it bakes in
   (`--instance-cpu`/`--instance-memory`).
2. **Snapshot capture is implicit (shutdown-only).** There is no way to say "snapshot
   the VM *now*, while it's warm, and let me resume it later by key." The VM snapshot
   only ever happens at prewarm-build time, for the controller's benefit.

This proposal makes both kinds of state **first-class, keyed, and client-driven**:
a sandbox has an explicit `sandbox.snapshot()` API; snapshots are split into a
`vm` part and a `files` part; and the client — not the controller — decides when
to capture and when to resume.

## 2. Goals / Non-goals

Goals:
- One snapshot config with two independent parts: `vm` (firecracker VM state) and
  `files` (the existing tar of writable FS).
- An explicit, server-implemented `sandbox.snapshot({vm, files})` action.
- VM state keyed by `vm.key`; a get-or-create resumes the keyed VM snapshot if one
  exists, else cold-boots. The client takes the snapshot after first warm-up.
- Files keyed by `files.key`; `files.write_on_shutdown: true` reproduces today's
  capture-on-shutdown behavior.
- `--snapshot-dir` stores **both** vm and files snapshots, on NVMe, bypassing the
  pod overlay.
- The **captured config is authoritative** on VM resume. A different config at
  resume time is ignored, except `entrypoint` / `env` / `cwd`, which may be
  honored — but only when they actually differ, because honoring them costs the
  warm captured entrypoint.
- Remove `--prewarm`, `--instance-cpu`, `--instance-memory` from `sandboxd`.

Non-goals:
- Live migration / cross-node resume (snapshots are node-local, NVMe-backed).
- VM snapshots in containers — `vm` is a **no-op** for the container backend.
- Changing the files tar format (still byte-compatible across backends).
- Solving warm-pool prefill policy. The client takes snapshots; *who* primes the
  cache and *when* is an orchestration concern above sandboxd.

## 3. Config schema

`restore_key` → `key`. `snapshot` splits into `vm` and `files`:

```jsonc
"snapshot": {
  "vm": {
    "key": "claude-code-agent-vm"
  },
  "files": {
    "write_on_shutdown": true,         // optional; default false
    "key": "claude-code-agent-files",
    "include": [
      "/home/agent/.claude/*",
      "/home/agent/.claude.json"
    ]
  }
}
```

Both parts are optional and independent. A spec may carry `vm` only, `files`
only, both, or neither.

`spec.Snapshot` ([spec.go:69](../../internal/spec/spec.go#L69)) becomes:

```go
type Snapshot struct {
    VM    *SnapshotVM    `json:"vm,omitempty"`
    Files *SnapshotFiles `json:"files,omitempty"`
}

type SnapshotVM struct {
    Key string `json:"key"` // matches snapshotKeyRE; noop in containers
}

type SnapshotFiles struct {
    Key             string   `json:"key"`
    Include         []string `json:"include,omitempty"`
    WriteOnShutdown bool     `json:"write_on_shutdown,omitempty"`
    Mount           string   `json:"mount,omitempty"` // unchanged: route tar to a FUSE drive
}
```

`Mount` (route the files tarball to an internal remote-backed FUSE drive, today's
`snapshot.mount`) stays on `files` only — a VM snapshot is large, mmap-resumed,
and NVMe-local; it is never written through 9p/FUSE.

### Compatibility

The flat `restore_key`/`write_key`/`include`/`mount` form is removed (this is
pre-1.0; the client and the chart move together). The mirror types in
[client/go/types.go:104](../../client/go/types.go#L104) and the generated
[config.gen.go](../../internal/api/gen/sandbox/config.gen.go) move in lockstep.
`spec.Validate` ([spec.go:449](../../internal/spec/spec.go#L449)) validates the
two sub-keys against `snapshotKeyRE` and keeps the `files.mount` ⊂ `fs[].mount`
check.

## 4. The `sandbox.snapshot()` API

A new server action, exposed at `POST /v1/<key>/snapshot`, handled alongside the
other sandbox actions in [internal/api/handlers](../../internal/api/handlers/sandbox.go).
Request body mirrors the config shape:

```jsonc
POST /v1/<key>/snapshot
{
  "vm":    { "key": "claude-code-agent-vm" },
  "files": { "key": "claude-code-agent-files",
             "include": ["/home/agent/.claude/*", "/home/agent/.claude.json"] }
}
```

Semantics:

- **`files`** — capture **now**, from the *running* workload. This is
  [FlushAgent](../../internal/isolation/isolation.go#L376) (sync the guest) +
  [CaptureSnapshot](../../internal/isolation/isolation.go#L312) into
  `SnapshotPath(snapshotDir, files.key)`. Works in both backends. Unlike
  `write_on_shutdown`, the workload keeps running afterward.
  - Open question (§8): the microvm `CaptureSnapshot` loop-mounts the overlay
    image and today assumes the guest is **powered off**
    ([microvm.go:658](../../internal/isolation/microvm.go#L658)). Capturing a
    *live* guest needs a quiesce-and-snapshot of the block device (e.g. take the
    VM snapshot below first, then read the overlay from the paused state), or a
    guest-side tar over the control channel. Recommended: derive the live files
    capture from the same paused VM snapshot, so `vm` + `files` are consistent.
- **`vm`** — pause the guest, write `snapshot.bin` + `mem.bin` + the overlay into
  `snapshotDir` keyed by `vm.key`, then **resume the guest in place**. This is the
  existing PrewarmSnapshot machinery ([microvm.go:1100](../../internal/isolation/microvm.go#L1100)),
  re-pointed from "build-time, controller-owned" to "request-time, client-owned,"
  and made resumable (firecracker can snapshot a running VM and continue).
- **`vm` in a container** — **no-op**. Return success with `vm: {captured:false,
  reason:"container backend"}` so clients can write backend-agnostic code.

Response reports, per part, whether it was captured and where:

```jsonc
{ "vm":    { "captured": true, "key": "...-vm" },
  "files": { "captured": true, "key": "...-files", "bytes": 12873 } }
```

## 5. Lifecycle: cold-first, client snapshots, key-keyed resume

This is the core behavioral change. It replaces the controller-driven prewarm/
prealloc orchestration with a **lazy, client-driven** model.

```
get-or-create(config with snapshot.vm.key = K)
        │
        ▼
   snapshotDir has vm snapshot for K?
        │                     │
       no                    yes
        │                     │
        ▼                     ▼
   COLD BOOT             RESUME K  (ignore incoming config; use captured config,
   from config           §6 reconciles entrypoint/env/cwd only)
        │
   (client warms it up, then)
        │
        ▼
   POST /v1/<key>/snapshot {vm:{key:K}, files:{...}}
        │
        ▼
   K now exists → next get-or-create resumes it
```

1. **No VM snapshot for `vm.key`** ⇒ the VM **always starts cold** from the
   supplied config (today's `launchWorkload`). No implicit prewarm, no base-snapshot
   build on the create path.
2. The **client** decides the VM is warm (its own readiness signal — e.g. the agent
   booted, browser is up) and calls `sandbox.snapshot({vm:{key:K}})`.
3. **Subsequent get-or-create with the same `vm.key`** resumes the captured VM
   ([ResumeAgent](../../internal/isolation/isolation.go#L346) /
   [ResumeReady](../../internal/isolation/isolation.go#L352)) instead of cold-booting.

The base-snapshot concept from `multi-sandbox-pod.md` becomes a *special case*:
a pod can pre-populate `snapshotDir` with a VM snapshot under a well-known key so
the first claim resumes instead of cold-booting — but that priming is now "call
snapshot once," not a distinct `--prewarm` code path.

## 6. Captured config is authoritative; entrypoint/env/cwd reconcile

A VM snapshot freezes the **entire machine**: kernel cmdline, guest sizing
(firecracker fixes vCPU/RAM in the snapshot), the running entrypoint process, and
the environment it booted with. Therefore:

- On resume, the **config used at capture time wins.** A different `image`, `cpu`,
  `memory`, `fs`, `egress`, etc. in the resume-time config is **ignored** — there is
  no way to retro-apply them to a frozen machine. (We log a warning when the resume
  config diverges from the captured one so this isn't silent.)
- The exceptions are **`entrypoint`, `env`, and `cwd`**, which the existing resume
  path already reconciles over the control channel
  ([ResumeState](../../internal/isolation/isolation.go#L197) →
  [ControlRequest](../../internal/firecracker/control.go#L50-L73)):
  - `env` is cheap and already applied (the guest needs PATH for exec resolution).
  - A changed `entrypoint`/`cwd` means **relaunching** the workload inside the
    resumed guest — which **throws away the warm captured entrypoint process** the
    snapshot's whole value rests on. This is expensive.

**Recommendation:** honor an `entrypoint`/`cwd` override only when it *actually
differs* from the captured one. Persist the captured config's entrypoint/cwd
alongside the snapshot (a small `snapshot-<key>.meta.json` next to the `.bin`
files); on resume, compare; if equal, **reuse the running captured entrypoint**
(no relaunch — the common case for a keyed warm-pool); if different, relaunch as
today and accept the cost. `env` is always merged (cheap), but should not by
itself trigger a relaunch.

This keeps the fast path (resume a keyed VM whose entrypoint matches) genuinely
fast, and makes the slow path (override the entrypoint) explicit and rare.

## 7. `sandboxd` flags & storage

Removed:
- `--prewarm` — replaced by lazy cold-boot + client `snapshot()`.
- `--instance-cpu` / `--instance-memory` — guest sizing now comes from the config
  at **cold-boot** time (`spec.cpu`/`spec.memory`, the same fields the container
  backend already uses) and is frozen into the VM snapshot from then on. There is
  no separate controller-injected sizing knob.

Kept / changed:
- `--snapshot-dir` — now stores **both** `files` tarballs (`snapshot-<key>.tar.gz`)
  and `vm` snapshots (`<key>/snapshot.bin`, `<key>/mem.bin`, `<key>/overlay.ext4`).
  It must be a directory that **skips the pod's overlay and passes through to
  NVMe** — VM mem images are large and mmap-resumed; serving them through the
  container overlay would be slow and would copy on write. In k8s this is a
  `hostPath`/local-PV NVMe mount; the chart wires it (see
  [values.yaml:82](../../deployment/k8s/chart/values.yaml#L82), which currently
  documents the now-removed `--instance-*` flags).
- `--prealloc-pool` — orthogonal (network slots), retained. It no longer implies a
  warm *workload*; it only pre-wires netns/veth/iptables. The warm workload now
  comes from a keyed VM snapshot resume.

`SnapshotPath` ([snapshot.go:185](../../internal/snapshot/snapshot.go#L185)) gains
a sibling for VM snapshots, e.g. `VMSnapshotDir(dir, key) = dir/<key>/`.

### Controller / runtime impact

[docker_runtime.go](../../internal/api/controller/docker_runtime.go#L367) and
[k8s_runtime.go](../../internal/api/controller/k8s_runtime.go) stop passing
`--prewarm` / `--instance-cpu` / `--instance-memory` / `--pack` prewarm args. The
prewarm-pod discovery + `packCache`
([pack_cache.go](../../internal/api/controller/pack_cache.go)) lose their reason to
exist in their current form: pods are no longer "parked prewarm hosts," they are
ordinary sandbox hosts whose first claim cold-boots and whose snapshots are taken
by the client. (Whether to keep a controller-level cache that *calls* `snapshot()`
to prime keys is a follow-up, not part of this proposal.)

## 8. Capture mechanics (microvm)

The tricky part is capturing a **consistent** `vm` + `files` pair from a *running*
guest, since today's microvm files capture assumes a powered-off guest and a
dirty-but-quiescent overlay ([withOverlayMount](../../internal/isolation/microvm.go#L677)).

Proposed sequence for `sandbox.snapshot({vm, files})` on microvm:

1. `FlushAgent` — sync the guest so the overlay block device is durable.
2. Pause the guest (firecracker `Patch /vm state=Paused`).
3. Write the VM snapshot (`snapshot.bin`/`mem.bin`) and freeze/copy the overlay
   into `snapshotDir/<vm.key>/`.
4. If `files` is requested, loop-mount the just-frozen overlay copy read-only and
   run [snapshot.Capture](../../internal/snapshot/snapshot.go#L53) against it (plus
   the local-FUSE backend dirs, which are host-side and already consistent).
5. Resume the guest in place.

This makes `files` derived from the same instant as `vm` (consistent), and avoids
loop-mounting an overlay that a live guest is still writing.

Container backend: `vm` is a no-op; `files` is the existing live capture (the
overlay upper is a plain host dir — no quiesce needed).

`write_on_shutdown: true` keeps the existing
[finalizeShutdown](../../cmd/sandboxd/main.go#L937) path, reading the (new) nested
`files` config: capture on shutdown only when `files.write_on_shutdown` is set.
Note the behavioral change — today shutdown capture is implicit whenever a write
key exists; now it requires the explicit `write_on_shutdown` flag.

## 9. Implementation sketch (incremental)

1. **Schema** — split `spec.Snapshot` into `vm`/`files` and update `Validate`.
   Migrate fixtures/specs in `test/e2e` and `docker/*`.
2. **Propagate the schema to every client surface** — the snapshot type is mirrored
   by hand across four language/tooling surfaces plus the OpenAPI source of truth.
   All must move together (this is a breaking, pre-1.0 rename), and the new
   `POST /v1/<key>/snapshot` action (§4) needs a method on each client:
   - **OpenAPI** (source of truth for Go codegen) — update the `Snapshot` schema
     and add the `/v1/{key}/snapshot` path in [api/config.yaml](../../api/config.yaml#L88)
     (and `api/sandbox_server.yaml` for the action), then regenerate
     [internal/api/gen/sandbox/config.gen.go](../../internal/api/gen/sandbox/config.gen.go).
   - **Go** — [internal/spec/spec.go](../../internal/spec/spec.go#L69) and the client
     mirror [client/go/types.go](../../client/go/types.go#L104); add a `Snapshot()`
     method on the Go sandbox client.
   - **TypeScript** — the zod schema + interface in
     [client/typescript/src/schemas.ts](../../client/typescript/src/schemas.ts#L252)
     and a `snapshot()` method on `Sandbox` ([sandbox.ts](../../client/typescript/src/sandbox.ts));
     refresh the [snapshot examples](../../client/typescript/examples/snapshot.ts).
   - **Python** — the pydantic model in
     [client/python/src/hiver/schemas.py](../../client/python/src/hiver/schemas.py#L120)
     and a `snapshot()` method on the sandbox client
     ([sandbox.py](../../client/python/src/hiver/sandbox.py)).
   - **Inspector** — the JSON-schema form in
     [cli/packages/inspector-client/src/sandboxConfigSchema.ts](../../cli/packages/inspector-client/src/sandboxConfigSchema.ts#L88),
     the config templates in
     [SandboxConfigTemplates.tsx](../../cli/packages/inspector-client/src/components/SandboxConfigTemplates.tsx),
     and any snapshot wiring in
     [transport.tsx](../../cli/packages/inspector-client/src/lib/transport.tsx) /
     inspector-server.
3. **Files-only first** — rename `restore_key`→`files.key`, gate shutdown capture
   on `files.write_on_shutdown`, point `maybeRestore`
   ([reconciler.go:142](../../cmd/sandboxd/reconciler.go#L142)) and
   `finalizeShutdown` at the nested config. No behavior change beyond the flag gate.
4. **`POST /v1/<key>/snapshot`** — wire a handler to `FlushAgent` +
   `CaptureSnapshot` for `files`; stub `vm` as captured:false in the container
   backend.
5. **VM capture/resume by key** — generalize PrewarmSnapshot/Resume to (a) write
   into `snapshotDir/<vm.key>/`, (b) resume the guest in place after capture, and
   (c) drive get-or-create off "does `vm.key` exist?" instead of `--prewarm`.
6. **Config-authoritative resume** — persist captured entrypoint/cwd metadata;
   compare on resume; only relaunch when entrypoint/cwd differ (§6).
7. **Drop flags** — remove `--prewarm`/`--instance-cpu`/`--instance-memory` and
   their controller/runtime/chart wiring; size the guest from `spec.cpu`/`.memory`.

Steps 1–4 are independently shippable and low-risk; 5–7 are the substantive change.

## 10. Open questions

- **Live VM snapshot resume-in-place**: confirm firecracker `CreateSnapshot` on a
  paused VM lets us `Resume` the same VMM (vs. only resuming a *fresh* VMM from the
  files). If not, `sandbox.snapshot({vm})` either briefly stops then resumes from
  disk, or we accept that taking a vm snapshot ends the live process.
- **Sizing mismatch on resume**: a resume-time config asking for different
  `cpu`/`memory` than the snapshot — warn-and-ignore (proposed) vs. reject with an
  error so the caller picks a different key.
- **Eviction / GC of `snapshotDir`**: keyed snapshots accumulate on NVMe. Out of
  scope here, but `--snapshot-dir` needs a size/age reaper eventually.
- **Atomicity of multi-part snapshot**: if `vm` succeeds but `files` fails (or vice
  versa), do we roll back the written key or report partial success? Proposed:
  per-part success in the response, no cross-part rollback.
