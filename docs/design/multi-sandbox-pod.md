# Design Proposal: Multi-Sandbox Pod (one host, many VMs/containers)

Status: Proposal
Author: (you)
Related: this supersedes the warm-pool / pod-per-sandbox model.

## 1. Motivation

Today the model is **one sandbox per pod**. Measurements this cycle showed the
real cost of a claim is *pod + control-plane lifecycle*, not the workload:

- A claim's `create` was 13–20s under burst, but the resume was ~1s and the
  browser ~0.4s — the rest was pod scheduling, warm-pool adopt churn, and k8s
  API pressure (`get-or-create handler total 20s` vs `claimed warm pod in 1s`).
- A firecracker snapshot **resume is cheap**: 10 concurrent `LoadSnapshot` = ~145ms
  wall, 20 = ~377ms. Idle resumed guests fault almost nothing (10 held = 67Mi
  total) because guest memory is lazily demand-paged (File backend).

So a pod that hosts **many microVMs/containers** amortizes pod + control-plane
overhead, makes claims local (resume a VM slot, no pod scheduling), and packs
idle/warm VMs cheaply. It does **not** add CPU/memory bandwidth — concurrently
*driven* sandboxes still contend on the node; that stays a capacity concern.

## 2. Goals / Non-goals

Goals:
- One pod runs one `sandboxd`, one `sbxfuse`, one `sbxproxy`, and **0..N** runc
  containers or firecracker VMs, all of the **same image**.
- Sandbox API keyed by sandbox `key`: `/v1/<key>/<resource|action>`.
- `sbxfuse` / `sbxproxy` shared, but policy/ACL-aware per sandbox.
- Snapshot/resume still works per VM.
- Remove the controller-managed prewarm pool.

Non-goals:
- Multiple images per pod (explicitly disallowed — see §5).
- Solving active-load node contention (orthogonal; still needs cores/hugepages).
- Cross-pod live migration.

## 3. Architecture overview

```
                 ┌─────────────────────────── sandbox pod (image X) ───────────────────────────┐
   client        │                                                                              │
     │           │   sandboxd (supervisor)            sbxproxy (shared)        sbxfuse (shared)  │
     ▼           │   ├─ map[key]→*Sandbox             ├─ srcIP → ACL           ├─ mount per key  │
  gateway        │   ├─ POST /v1/<key> (create)       │  (per-sandbox egress)  │  + per-mount    │
 /sandbox/<id>/  │   ├─ GET/POST /v1/<key>/...        │                        │    ACL          │
     │           │   ├─ IP/tap/cgroup/vsock alloc     └────────┬───────────────┴───────┬─────────┘
     ▼           │   └─ base snapshot (image X)                │ egress                 │ 9p/vsock
 pod IP (=<id>)  │        │ resume per VM                       │                        │
                 │   ┌────┴─────┐   ┌──────────┐   ┌───────────┴┐                        │
                 │   │ VM key=a │   │ VM key=b │   │ ctr key=c  │  ◄─────────────────────┘
                 │   └──────────┘   └──────────┘   └────────────┘                        │
                 └──────────────────────────────────────────────────────────────────────┘
```

- The pod is identified to the controller/gateway by an **id** (k8s: pod IP in
  hex, as today; docker: container uuid). `<id>` routes to the pod.
- Inside the pod, `<key>` routes to a specific sandbox.

## 4. Identity & addressing

- **Host id** (`<id>`): addresses the *pod*. k8s = pod IP in hex (`ipID`, as
  today). docker = container uuid.
- **key**: addresses a *sandbox within the pod*; unique per pod. Caller-chosen
  (e.g. `agent-1`).
- Client URL: `/sandbox/<id>/v1/<key>/<resource>` (§9, §10).
- Gateway: `/sandbox/<id>/...` → pod at `<id>` (unchanged routing); the
  `/v1/<key>/...` suffix is handled inside the pod (§8).

## 5. sandboxd as a supervisor

`cmd/sandboxd` changes from "one process = one sandbox" to a **supervisor** that
owns a map and the shared sidecars.

```go
type Supervisor struct {
    image     string                 // fixed for the pod; first create sets it
    mu        sync.Mutex
    sandboxes map[string]*Sandbox    // key -> sandbox (req 4)
    ipAlloc   *IPAllocator           // pod-local guest subnet
    proxy     *ProxyControl          // push per-sandbox egress ACLs to sbxproxy
    fuse      *FuseControl           // add/remove per-sandbox workspaces in sbxfuse
    base      *BaseSnapshot          // image's warm template (§7), optional
    capacity  Capacity               // admission control (§6)
}

type Sandbox struct {
    key      string
    iso      isolation.Isolation     // its own VM/container (microvm or container)
    cfg      *spec.Spec
    guestIP  net.IP                  // pod-local
    tap      string                  // fctap-<short(key)>
    cgroup   string                  // per-VM cpu/mem limits
    vsockUDS string                  // per-VM control/exec/file channel
    life     *api.Lifetime           // per-sandbox idle TTL
    state    State                   // creating|running|stopping|stopped
}
```

Key points:
- The **isolation backend stays per sandbox** — `internal/isolation` microvm /
  container is instantiated per `Sandbox`, each with its own tap, overlay/jail,
  cgroup, vsock UDS, snapshot files. No isolation-layer rewrite; the change is
  *who owns/instantiates them* (supervisor, not a singleton main).
- **Same-image invariant (req 3):** the pod's image is fixed at first create
  (or from boot env). `POST /v1/<key>` with a different image → `409 Conflict`.
  This lets all VMs share one base snapshot, one rootfs drive, one image pull.

### 5.1 API surface (req 1, 2)

All sandbox routes are prefixed by `<key>`:

| Method & path | Action |
|---|---|
| `POST /v1/<key>` | Create a VM/container for `key` from the pod's image (req 2). Body = config (spec). Errors if image mismatch (req 3) or capacity exceeded. |
| `GET  /v1/<key>/config` | Per-sandbox config (was `/v1/config`). |
| `PUT  /v1/<key>/config` | Update config (env/workspaces/egress); re-registers ACLs. |
| `POST /v1/<key>/exec` (stream) | Exec into that sandbox (vsock to its VM). |
| `*    /v1/<key>/file*` | File ops on that sandbox. |
| `GET  /v1/<key>/events` | Per-sandbox event stream. |
| `DELETE /v1/<key>` | Tear down that sandbox, free its slot. |
| `GET  /v1/` | List sandboxes in the pod (ops/debug). |
| `GET  /v1/ping` | Pod-level readiness (sandboxd up). |

Dispatch: a gin middleware extracts `<key>`, looks up `sandboxes[key]`, attaches
the `*Sandbox` to the request context, 404s on unknown key. Existing handlers
(`internal/api/handlers`) become methods that operate on the resolved `*Sandbox`
instead of process-wide singletons.

## 6. Resource management & admission

- **IP/tap allocation:** pod-local subnet (e.g. `172.16.0.0/24`, gateway `.1`,
  guests `.2…`). `IPAllocator` hands out a guest IP + tap (`fctap-<n>`) per
  sandbox in the pod netns (the experiment confirmed per-netns tap creation
  works; here it's one shared netns with N taps).
- **Per-VM cgroups:** the firecracker `cgroupWrap` already places each VM in its
  own cgroup; the supervisor sets that cgroup's cpu/mem from the per-sandbox
  config. This restores per-sandbox limits that k8s no longer provides
  (k8s limits are now per-*pod*).
- **Admission control:** the pod advertises a capacity (e.g. N vCPU-equiv, M
  GiB). `POST /v1/<key>` is admitted only if the pod has headroom; otherwise
  `429`/`507` so the controller creates/uses another pod for the image. This is
  the in-pod scheduler the multi-VM model requires.
- **Lifetime:** per-sandbox idle TTL (reuse `api.Lifetime`) tears down a single
  sandbox and frees its slot; the pod persists for the image.

## 7. Snapshot, resume, and fast VM start (req 6, 11)

Removing the prewarm **pool** (req 11) removes the controller's pool of warm
*pods*. We keep the *speed* by moving "warm" **inside the pod** as a per-image
**base snapshot**:

1. On first need, the pod cold-boots the image's prewarm once and
   `CreateSnapshot`s it → `base/{snapshot.bin, mem.bin}` (the image template).
2. Each `POST /v1/<key>` **resumes** the base snapshot into a fresh VM:
   per-VM CoW `overlay.ext4`, per-VM tap/IP/vsock, **shared `base/mem.bin`** via
   the File backend (mmap is COW-private per VM). Measured cost: tens of ms, and
   idle VMs share the base memory (the density win — §1).
3. Post-resume, `ApplyResumeState` delivers that sandbox's env/workspaces/clock
   over its vsock control channel (with the self-heal retry already designed:
   idempotent `mountWorkspaces`, re-drive until the guest confirms).

Per-sandbox **save/restore** (the existing `spec.Snapshot` RestoreKey/WriteKey)
still works — each VM can snapshot/restore its own state independently; that is
unchanged and orthogonal to the base template.

> This keeps `internal/isolation/microvm.go`'s snapshot/resume machinery; what
> changes is that resume is now the *normal create path within a pod*, not a
> warm-pool claim. If the base-snapshot optimization is deferred, `POST` simply
> cold-boots a VM — correctness is identical, only slower.

## 8. sbxproxy — shared, per-sandbox egress ACL (req 5)

`sbxproxy` serves all VMs in the pod and must apply **the originating sandbox's**
egress policy. Mapping key = **source guest IP** (each sandbox has a distinct
pod-local IP).

- Rules file becomes keyed by source: `srcIP → []EgressRule` (today it's a flat
  `egress-rules.json`). sandboxd writes the merged map and signals reload
  (sbxproxy already hot-reloads ACLs).
- On each connection, sbxproxy resolves `srcIP → sandbox policy`; unknown source
  → deny. Audit events carry the sandbox `key`.
- One listener, one CA, shared TLS-MITM machinery — only the policy lookup
  becomes per-source.

## 9. sbxfuse — shared, multi-workspace, per-mount ACL (req 5)

`sbxfuse` serves all sandboxes' workspaces from one process:

- Per sandbox+workspace: a host FUSE mount `/workspace/<key>/<name>` backed by a
  per-sandbox backend dir, with that sandbox's fs ACLs (sbxfuse already supports
  per-mount ACL files).
- The **9p-over-vsock export is per VM** (rooted at that sandbox's FUSE mount,
  served over that VM's vsock) — so guests stay isolated; only the host process
  is shared. `ExportWorkspace`/`mountWorkspaces` keep their shape, just scoped by
  sandbox.
- sandboxd's `FuseControl` adds/removes a sandbox's workspaces as it is
  created/destroyed; ACL updates reload in place.

## 10. Controller `getOrCreate` (req 10) & warm-pool removal (req 11)

The controller now provisions **hosts keyed by image**, and per-sandbox creation
moves to the pod:

- **k8s (10.2):** `getOrCreate(image)`: key = `hash(image)`. If a pod for that
  hash exists → return its **IP in hex** as the id (as today, `ipID`). Else
  create the pod (one pod per image). The pod is named/labeled by `hash(image)`,
  so pod creation is the atomic per-image lock.
- **docker (10.1):** key = `hash(image + key)`. If the container exists → return
  its **uuid**. (Local/dev path: a host per (image,key); the same keyed sandbox
  API runs inside, typically with one entry.)
- **Remove** `internal/warmpool` (manager, index, CRD, the reconciler, the
  recycler's pool bits) and the WarmPool manifests. The pod itself is the unit
  the controller manages; "warm" lives inside the pod as the base snapshot (§7).
- The controller no longer claims/relabels per-sandbox pods; the heavy
  `claimWarm` adopt path and its k8s API churn disappear (the measured 19s).

Client → controller `getOrCreate(image)` returns `<id>`; the client then
`POST /sandbox/<id>/v1/<key>` to create the sandbox, then drives it by key.

## 11. Gateway routing (req 7, 8)

- Client sends `/sandbox/<id>/v1/<key>/<resource>`.
- Gateway: match `/sandbox/<id>/`, route to the pod at `<id>` (k8s: resolve hex
  id → pod IP; same mechanism as today), forward the `/v1/<key>/...` remainder
  unchanged.
- No per-sandbox routing state in the gateway — it still routes by `<id>` to the
  pod; the pod fans out by `<key>`. This keeps the gateway simple.

## 12. Clients (req 9)

ts / py / go clients change shape:

```
getOrCreateSandbox(key, { image }):
  id = controller.getOrCreate(image)              # host id (pod IP-hex / uuid)
  POST /sandbox/{id}/v1/{key}        {config}      # create sandbox in the host (req 2)
  return Sandbox{ id, key }                        # carries (id, key)

sandbox.exec/file/config/shutdown:
  -> /sandbox/{id}/v1/{key}/<resource>             # keyed path (req 8)
```

- The `Sandbox` object holds `(id, key)` and builds keyed URLs.
- `shutdown(sandbox)` → `DELETE /sandbox/{id}/v1/{key}`.
- Update the three clients + examples (e.g. `benchmark-browser-resident.ts`
  becomes "getOrCreate host once per image, then POST N keyed sandboxes").

## 13. Failure domains, security, isolation

- **Guest isolation is unchanged** — each sandbox is its own firecracker VM (or
  runc container); the VM boundary is the security boundary and it still holds.
- **Larger host blast radius:** a pod death (OOM, node drain, crash) takes all
  its VMs. Mitigate with per-VM supervision, conservative pod sizing, and
  admission limits so one pod isn't overpacked.
- **Shared host surface:** one sandboxd/sbxproxy/sbxfuse now serve N sandboxes —
  a host-side bug affects N, not 1. Keep per-sandbox ACL enforcement strict
  (default-deny on unknown source/key) and audit by key.
- **noisy neighbor:** per-VM cgroups bound CPU/mem; admission bounds count.

## 14. Migration plan (phased)

1. **Refactor in place:** extract a `Sandbox` type and a `Supervisor` in
   `cmd/sandboxd` while still running one sandbox per pod (no behavior change).
2. **Keyed API:** add `/v1/<key>/...` and `POST /v1/<key>`; keep the old routes
   as `key="default"` shims for one release.
3. **Shared sidecars:** make sbxproxy per-source and sbxfuse multi-mount.
4. **Per-VM alloc:** IP/tap/cgroup/vsock allocators; admission control.
5. **Base snapshot:** in-pod resume-per-VM fast start.
6. **Controller:** `getOrCreate(image)` (k8s) / `hash(image+key)` (docker);
   delete `internal/warmpool` + manifests.
7. **Gateway + clients:** keyed routing; ship ts/py/go updates + examples.
8. **Remove shims.**

## 15. Open questions / risks

- **Base-snapshot CoW sharing**: confirm File-backend `mem.bin` shared read by N
  firecrackers is COW-private (experiment suggests yes); validate at scale and
  watch page-cache pressure.
- **Admission policy / pod sizing**: how many VMs per pod, and how the controller
  spreads load across pods of the same image (capacity feedback from the pod).
- **Per-source ACL granularity** in sbxproxy when sandboxes share an image but
  have different egress policy — confirm srcIP is always a reliable discriminator
  (it is, given per-VM IPs).
- **docker semantics**: `hash(image+key)` means docker hosts are effectively
  per-sandbox; decide whether docker should also be multi-VM or stay 1:1 for dev.
- **Active-load contention** is unchanged — packing more VMs/pod doesn't add
  cores; pair with hugepages (cheaper resume faulting) and node sizing.

## 16. Requirements coverage

| # | Requirement | Section |
|---|---|---|
| 1 | 1 sandboxd / sbxfuse / sbxproxy, 0..N VMs/ctrs | §3, §5, §8, §9 |
| 2 | `POST /v1/<key>` creates VM/ctr by image | §5.1, §7 |
| 3 | same image per pod, else error | §5, §5.1 |
| 4 | sandboxd in-memory key→resources map | §5 |
| 5 | sbxfuse/sbxproxy shared + per-sandbox ACL | §8, §9 |
| 6 | snapshot/resume works for firecracker | §7 |
| 7 | gateway routes `/sandbox/<id>/` to pod | §11 |
| 8 | clients call `/sandbox/<id>/v1/<key>/...` | §4, §11, §12 |
| 9 | ts/py/go clients updated | §12 |
| 10 | getOrCreate: docker hash(image+key)→uuid; k8s hash(image)→IP-hex | §10 |
| 11 | remove prewarm pools | §7, §10 |
