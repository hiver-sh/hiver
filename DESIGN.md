# Agent Sandbox Design Document

This document describes the implementation design for the Agent Sandbox platform whose functional requirements are specified in [PRD.md](PRD.md). It is the blueprint for building the system in Go, persisting state in PostgreSQL, and deploying identically on developer laptops (macOS, Linux), on-premise hardware, and cloud Kubernetes clusters.

> **Traceability.** Each requirement in [PRD.md](PRD.md) carries a stable `REQ-N` identifier. Individual statements in this document that implement a requirement end with an inline `(REQ-N)` tag (or `(REQ-N, REQ-M)` when a single statement covers multiple). To find every place the design satisfies a given requirement, `grep "REQ-N\b" DESIGN.md`.

## 1. Goals & Non-Goals

### Goals

- Provide a single declarative API for spinning up isolated, ephemeral execution environments for AI agents.
- Mediate **all** filesystem and network egress through platform-controlled enforcement points (FUSE, MITM proxy).
- Produce a tamper-evident, replayable audit trail of every agent action.
- Run unchanged on macOS / Linux laptops and Kubernetes clusters.
- Make the safe path the easy path: deny-by-default policies, brokered credentials, fail-closed components.

### Non-Goals

- Running untrusted agent _binaries_ with kernel-level guarantees on shared hosts. v1 ships `runc` only; the host kernel is in scope for the threat model. Kernel-level isolation against hostile workloads requires a different runtime (gVisor / Kata / Firecracker), which we treat as future work (§3.3, §18). Customers needing those today should run on a single-tenant cluster or a hosted VM-grade offering.
- Replacing existing IAM/secrets systems. The platform integrates with HashiCorp Vault, AWS KMS, GCP KMS, and OIDC IdPs rather than reimplementing them.
- Real-time GPU multi-tenancy. v1 supports CPU/memory limits; GPU support is scoped for a follow-up.

## 2. High-Level Architecture

The architecture splits cleanly into a **control plane** (multi-tenant, cluster-wide, talks to PostgreSQL) and a **data plane** (per-sandbox, on a single node, mediates every agent action).

<img src="./architecture.svg" />

### 2.1 Process model

Every sandbox materializes as a **pod-like group** of three colocated processes sharing a network namespace and a workspace mount:

### Process model

Every sandbox instance materializes as a **pod-like group** of three colocated processes:

| Component       | Privilege                | Responsibility                                                                 |
| --------------- | ------------------------ | ------------------------------------------------------------------------------ |
| Agent container | Unprivileged, no `CAP_*` | Runs the user's agent. No direct network or host filesystem access.            |
| MITM proxy      | Net-namespace owner      | Sole egress path. TLS-intercepts, enforces egress policy, brokers credentials. |
| FUSE daemon     | Owns workspace mount     | Mediates every fs syscall. Enforces ACLs, quotas, COW, scanning.               |

The agent's network namespace is wired so the proxy is the only reachable host (default route → proxy listener); the agent's `/workspace` is the FUSE mount point.

## 3. Components

### 3.1 Control-Plane API Server (`cmd/apiserver`)

- **Language**: Go 1.23+.
- **Frameworks**: `connectrpc.com/connect` for transport (serves both gRPC and HTTP/JSON from one handler), `chi` for non-RPC routes (health, metrics, admin), `buf` for IDL tooling. (REQ-1)
- **Auth middleware**: OIDC (verifies ID tokens against tenant-bound JWKs), mTLS (cert subject → tenant), and signed API keys (Ed25519). All three resolve to a `Principal{TenantID, ActorID, Roles[]}`. (REQ-11)
- **Authorization**: in-process OPA evaluator (`open-policy-agent/opa`) with policies stored as code under `policies/`. Every API call evaluates `data.sandbox.allow` with `{principal, action, resource}` and the result is logged. (REQ-11, REQ-12)
- **Validation**: spec validation runs in two stages: (1) JSON-schema/protobuf field validation, (2) semantic validation (e.g., requested egress domains match an allowed pattern; requested resource limits within tenant quota). (REQ-2, REQ-13)
- **Idempotency**: `Idempotency-Key` header (or gRPC metadata) hashed with the request body and stored in a `idempotency_keys` table. Repeat calls within a 24 h window return the original response. (REQ-6)
- **Spec resolver**: when the request references a `profile`, the server merges the named profile with inline overrides, runs validation, and stores the _resolved_ spec on the sandbox row. Patches operate on the resolved spec. (REQ-15)
- **Streaming endpoints** (`/logs?follow`): served as Server-Sent Events for HTTP and bidi streams for gRPC. Backed by a fan-out subscriber on the audit bus. (REQ-9, REQ-65)

### 3.2 Scheduler (`cmd/scheduler`)

A control-loop reconciler. Watches `sandboxes` rows in `desired_state ∈ {pending, running, deleted}` and drives them through a state machine:

```
pending → placing → starting → running → draining → terminated
                                   │
                                   └─→ failed (with reason)
```

- Placement: in Kubernetes, the scheduler creates a `Sandbox` CRD; a custom controller materializes it as a Pod with the agent + sidecars. Locally, the scheduler talks to a `sandboxd` daemon over the same gRPC + mTLS channel used in K8s — one transport, one auth model, in every environment.
- Heartbeats: each sandbox's `sandboxd` reports liveness every 5 s; missing 3 heartbeats marks the sandbox `failed`.
- Leases: scheduler instances elect a leader via a PostgreSQL advisory lock so multiple replicas can run for HA without double-acting on the same sandbox.

### 3.3 Runtime Agent (`cmd/sandboxd`)

The node-local daemon that performs the actual sandbox lifecycle work. One per node.

- Pluggable **isolation backend** behind a single Go interface:
  ```go
  type Runtime interface {
      Create(ctx context.Context, spec *Spec) (Handle, error)
      Start(ctx context.Context, h Handle) error
      Exec(ctx context.Context, h Handle, req ExecRequest) (ExecResult, error)
      Stop(ctx context.Context, h Handle, grace time.Duration) error
      Destroy(ctx context.Context, h Handle) error
  }
  ```
  This abstracts the runtime so additional backends can be added later without an architectural change. (REQ-17, REQ-61)
- Implementation in v1: `runtime/runc` (OCI-compatible). The interface is in place for future backends (`gvisor` is the most likely second pick); see "Isolation backend" below and §18. (REQ-2, REQ-22)
- Spawns the per-sandbox MITM proxy and FUSE daemon as siblings _before_ starting the agent container, so by the time the agent runs the egress path and workspace are already mediated. (REQ-23, REQ-48)
- Applies per-spec resource limits via cgroups v2 (CPU, memory, pids, IO) and a wall-clock killer goroutine for `resources.timeout`. (REQ-18)
- Drops privileges on the agent container per `security`: non-root user, empty capability set, `readOnlyRoot`, mounted seccomp profile. (REQ-20, REQ-21)
- Exposes a thin gRPC API to the scheduler for create/start/exec/stop/destroy and to stream stdout/stderr to the audit bus. (REQ-10)
- **Preflight gates at startup**: cgroup v2 (refuse v1 or hybrid) and active LSM matches the shipped profile (AppArmor on Debian/Ubuntu lineage, SELinux on RHEL/Fedora lineage). One-line error and exit on mismatch — no degraded-mode start. The same checks back `bin/sandbox doctor` for laptop installs.
- **LXCFS** is bundled into the sandbox base image and bind-mounted into agent containers, so language runtimes (Java/Go/Python) size thread pools and buffers to the sandbox's cgroup limits rather than the host's `/proc`.

#### Isolation backend

`spec.isolation` is the low-level OCI runtime that executes the prepared rootfs. **v1 ships `runc` only**; the spec validator rejects any other value.

`runc` is a low-level OCI runtime — it sets up Linux namespaces and executes the bundle. The layer above (image pull, layer unpack via overlayfs snapshotter, rootfs preparation) is owned by **containerd** in production (K8s, on-prem; §11.2, §11.3) or **Docker** locally (§11.1, laptop-only). The platform's per-sandbox `/workspace` is *separate* from the image rootfs — it's a FUSE mount (§3.5) that `sandboxd` bind-mounts into the rootfs after preparation but before the agent process starts (§8.1).

**Why `runc` is sufficient for v1.** The platform's value is the *boundaries we own* — MITM proxy for egress (§3.4), FUSE daemon for filesystem (§3.5), tamper-evident audit chain (§9), and userns / dropped capabilities / seccomp / AppArmor or SELinux on the agent process (§3.3, §14). The runtime layer's job is just to set up the namespaces and execute the bundle. `runc` does this with the smallest memory floor (~10 MB), the fastest cold start (10–50 ms), and no exotic host requirements — cgroup v2 is the only one, and it's available on every supported Linux host and inside the Linux VM on macOS.

**The trade-off.** `runc` shares the host kernel: a CVE the agent can reach through the seccomp-narrowed syscall surface gives host access. We accept this in v1 because (a) the platform's other boundaries do not depend on the runtime layer for their enforcement, (b) the reference threat model treats the host kernel as in-scope-trusted, and (c) shipping VM-grade alternatives (`gvisor`, `kata`, `firecracker`) would multiply the host-environment matrix (nested-virt SKUs, `/dev/kvm` exposure, per-backend shims, per-backend incompatibility lists) for marginal gain in most deployments. Customers with stricter threat models should run on a single-tenant cluster or use a hosted offering with VM-grade isolation (GKE Sandbox, AWS Fargate).

The `Runtime` interface in `cmd/sandboxd` is preserved so a second backend can be added later without an architectural change. `gvisor` is the most likely candidate (process-level, no nested-virt requirement, drops in via a containerd shim). See §18 for the evaluation of alternatives.

#### From image to rootfs: a worked example

Both layers — image pull/unpack and OCI execution — are reachable from the command line, which is the easiest way to understand what `sandboxd` does on your behalf.

The example below needs a **Linux kernel**: `runc`, overlayfs, namespaces, and cgroups don't exist on Darwin. On macOS every command runs *inside* the Linux VM that Docker / OrbStack provides — `docker pull` reaches a Linux daemon in the VM, `runc run` executes on the VM's kernel, not on the Mac host. From a terminal it looks transparent; mentally, swap *host* for *VM* whenever you read it. To run `runc` directly on a Mac (step 5), you'll need to `orb shell` or `docker run --privileged --rm -it debian` first to get a Linux process to execute it.

On Linux (or inside that VM):

```bash
# 1. Pull. Docker (or containerd) fetches the manifest, verifies each
#    layer digest, pulls the gzipped layer blobs, and pulls the image config.
docker pull alpine:3.20

# 2. Unpack. Layers are extracted into an overlayfs stack; the merged view
#    is a complete Linux rootfs that any OCI runtime can execute.
docker image inspect alpine:3.20 --format '{{json .GraphDriver.Data}}' | jq
# { "LowerDir":  "...",                                  ← read-only image layers
#   "MergedDir": "/var/lib/docker/overlay2/abc/merged",  ← this is a rootfs
#   "UpperDir":  "/var/lib/docker/overlay2/abc/diff",    ← per-container writable
#   "WorkDir":   "..." }

# 3. Get a flat rootfs you can hand to a runtime directly.
cid=$(docker create alpine:3.20 sh)
docker export "$cid" -o alpine-rootfs.tar
docker rm "$cid"
mkdir -p bundle/rootfs
tar -xf alpine-rootfs.tar -C bundle/rootfs

# 4. Build the OCI bundle. config.json describes namespaces, uid, args, mounts.
cd bundle
runc spec    # generates a default config.json next to rootfs/

# 5. Execute — no Docker in the loop. This is the layer §3.3's backends own.
sudo runc run my-sandbox
```

The `bundle/` directory is OCI-spec compliant. v1 ships `runc` only; the bundle format is what would let a future `gvisor` (or `kata`/`firecracker`) backend reuse the same preparation pipeline (§18).

In production, `sandboxd` doesn't shell out to `docker` or `tar`. It calls containerd's CRI API, which keeps layer blobs deduplicated across sandboxes (the read-only lower dirs are shared; only each sandbox's upper dir is unique — a major disk and page-cache win at density). The platform's per-sandbox `/workspace` (§3.5) is bind-mounted into the rootfs **after** step 4 and **before** step 5, so the FUSE-mediated workspace is in place by the time the agent process starts.

Caveats if you reproduce this:

- The example assumes the `overlay2` storage driver. Rootless Docker on `vfs` doesn't expose `MergedDir`, and each container is a full copy rather than an overlay.
- `docker export` flattens layers — fine for the demo, but defeats containerd's deduplication. Don't do this in production paths.

#### Same flow on Kubernetes (containerd)

In production the user-facing entry point is `kubectl apply`, and the platform's `Sandbox` CRD controller emits the Pod manifest. Steps 1–4 of the pipeline above are done by containerd in response to a CRI request from kubelet; step 5 is done by `runc` (containerd's default — no `RuntimeClass` needed for the supported configuration).

```bash
# 1. Apply a Pod. The CRD controller does this for real sandboxes; this
#    hand-rolled version exists for poking around. With no runtimeClassName
#    set, containerd uses runc — the v1 supported runtime.
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata: { name: alpine-sandbox }
spec:
  containers:
  - { name: agent, image: alpine:3.20, command: ["sleep","3600"] }
EOF
```

To inspect what containerd produced, drop to the node where the Pod landed and use `crictl` (the CRI client kubelet would use) and `ctr` (containerd's namespace-scoped CLI):

```bash
# 2. Image pulled and unpacked. Snapshots are namespaced under "k8s.io"
#    when the request comes from kubelet.
sudo ctr -n k8s.io image ls | grep alpine
sudo ctr -n k8s.io snapshot ls
# KEY                 PARENT                 KIND
# alpine-sandbox      sha256:layer-N         Active     ← per-container upper
# sha256:layer-N      sha256:layer-N-1       Committed  ← shared image layer

# 3. The merged rootfs the shim handed to runc — same shape as Docker's
#    MergedDir, different path.
sudo crictl ps --name alpine-sandbox
# CONTAINER     IMAGE         RUNTIME    ID
# abc123def     alpine:3.20   runc       ...

ls /run/containerd/io.containerd.runtime.v2.task/k8s.io/abc123def/rootfs/
# bin  dev  etc  lib  usr  var  ...

# 4. The OCI runtime spec containerd built (equivalent of `runc spec` output).
sudo crictl inspect abc123def | jq '.info.runtimeSpec | {process, root}'

# 5. Layer dedup is visible across sandboxes from the same image:
kubectl run alpine-sandbox-2 --image=alpine:3.20 -- sleep 3600
sudo ctr -n k8s.io snapshot ls
# alpine-sandbox     sha256:layer-N    Active   ← parent shared
# alpine-sandbox-2   sha256:layer-N    Active   ← parent shared
# Read-only image layers exist once on disk; only the per-container Active
# layer is unique. This is the win `docker export` throws away.
```

The platform's per-sandbox `/workspace` is then bind-mounted into the agent container by `sandboxd` via the FUSE CSI driver (§3.5, §11.2), the same way it does on the laptop — Docker socket replaced by CRI. Because the bundle is OCI-spec compliant, adding a second isolation runtime later (e.g. `gvisor` via `containerd-shim-runsc-v1`) is a `RuntimeClass` change and a spec-validator allow-listing, not a code change to `sandboxd` (§18).

#### Multiplexing: one sandboxd, many sandbox Pods

A node hosts many sandbox Pods, and the single sandboxd on that node manages all of them. Every gRPC call carries a `Handle`; sandboxd keeps a per-handle state map and creates the per-sandbox cgroup, netns, and FUSE mount as siblings under its own roots. Capacity per node is bounded by the cluster scheduler's resource accounting on the Pod spec, not by sandboxd itself. The shape:

- **N:1 multiplexing.** On node X, sandboxd handles every sandbox Pod scheduled to X. Sandboxes on node Y are managed by Y's sandboxd — there is no cross-node call path between sandboxd instances.
- **Colocation is structural, not network-routed.** When a sandbox Pod is scheduled to node X, the kubelet's CSI plumbing reaches the FUSE driver on X, and the cgroup / namespace setup runs in X's kernel. There is no scenario where a Pod on X is "managed by" sandboxd on Y. (See §11.2 for the matching `nodeSelector` between the DaemonSet and the sandbox Pod template that prevents Pods from landing on a node without sandboxd.)
- **Audit isolation holds across multiplexing.** Every event still carries `(tenant_id, sandbox_id)` so per-sandbox and per-tenant invariants survive even though the producer process is shared. (REQ-69)
- **Blast radius of a sandboxd crash.** Lifecycle management for *every* sandbox on the node hangs until sandboxd restarts. The sandbox Pods themselves keep running (their containers are independent processes); new exec calls and teardowns block. This is distinct from a per-sandbox FUSE-daemon crash (§3.5), which is fail-closed for one sandbox only.

### 3.4 MITM Proxy (`cmd/sbxproxy`)

- Built on `github.com/elazarl/goproxy` for the HTTP layer plus a thin custom TLS interceptor that uses a per-sandbox CA loaded at startup. The proxy is the sandbox's only egress path. (REQ-23, REQ-25)
- The CA is generated per-sandbox by `policysvc` (never reused across tenants) and the private key is mounted from a `tmpfs` volume only the proxy can read; the public cert is the only thing exposed to the agent. (REQ-26, REQ-27)
- Policy evaluator runs OPA-as-lib with the sandbox's egress block compiled to a Rego module; decisions are cached in-memory keyed by `(host, method, path-prefix)`. Default verdict is deny; allow rules come from the spec's per-task egress block. (REQ-31, REQ-32)
- Body inspection is streaming: requests/responses pass through pluggable inspectors (`secrets`, `pii`, `prompt-injection`, `clamav`) that share a `Reader` chain so we don't buffer entire bodies unless an inspector requests it. (REQ-44)
- Upload size cap and content-type allow/deny lists are enforced before bodies are forwarded; violations produce a 413/415 and a deny event. (REQ-34, REQ-38)
- Inbound auth-like headers (`Authorization`, `Cookie`, `X-Api-Key`, …) are handled per the egress block's `inboundAuthHeaders` field — `strip` (default), `reject`, or `passthrough`. (REQ-36)
- Credential broker: at sandbox creation, the API resolves each credential reference and pushes the secret material into the proxy's keyring over the same gRPC + mTLS channel used elsewhere. The proxy attaches headers per the egress block's `credentialBindings`. Credentials never touch disk and never appear in any log (audit log entries store an HMAC-SHA256 of the value with a per-sandbox salt). (REQ-35, REQ-37, REQ-42, REQ-43)
- Bandwidth and request-rate limiting via leaky-bucket per `(sandbox, destination)` and per tenant. (REQ-33)

### 3.5 FUSE Daemon (`cmd/sbxfuse`)

- The agent's `/workspace` is a userspace FUSE mount; every read/write/stat/unlink goes through this daemon, never the kernel directly. (REQ-48)
- Uses `bazil.org/fuse` on Linux behind a thin abstraction (`fs.Mounter`). There is no host-side macOS path: on developer Macs the daemon runs *inside* the Linux dev VM (OrbStack) and uses the same Linux mounter, so behavior and audit format match production exactly. macFUSE (kext + Reduced Security) and FUSE-T (NFSv4-loopback semantics) are both rejected — see §11.4 and the macOS limits in §17.x. (REQ-55)
- The daemon runs out-of-process and unprivileged; if it crashes the kernel returns `EIO` for every workspace op and `sandboxd` pauses the agent until the daemon restarts. (REQ-52)
- Pluggable **backend** interface:
  ```go
  type Backend interface {
      Open(ctx context.Context, path string, flags int) (File, error)
      ReadDir(ctx context.Context, path string) ([]Entry, error)
      Lookup(ctx context.Context, path string) (Attr, error)
      // ... rename, unlink, mkdir, symlink, etc.
  }
  ```
- Implementations: `backend/local` (host directory with a chroot-like jail), `backend/s3`, `backend/gcs`, `backend/encrypted` (age-encrypted blocks over any of the above) — policy is backend-agnostic. (REQ-53)
- Only the workspace path declared in the spec is exposed to the agent; sensitive host paths (`~/.ssh`, `~/.aws`, etc.) are never mounted. (REQ-45, REQ-46)
- ACL layer sits _above_ the backend: every op is checked against the per-path policy compiled from the spec's `filesystem` block. Denied paths return `ENOENT` (not `EACCES`) to avoid revealing existence. (REQ-49)
- Every op (`read`, `write`, `unlink`, `rename`, `chmod`, `setxattr`) emits an audit event on the same bus as network and process events for unified replay. (REQ-50)
- COW overlay: writes go to an upper layer (a tmpfs-backed `local` backend); reads first check the upper layer, then fall through to the read-only base. On session end the upper layer is destroyed; if `persistArtifacts` is set, listed paths are copied to the artifact bucket first via a signed URL. (REQ-19, REQ-47, REQ-51)
- Quotas tracked via in-memory counters checkpointed to PostgreSQL every 30 s; on restart the daemon re-walks the upper layer to rebuild counters. (REQ-18)
- Inline scanning runs on writes via the same inspector chain as the proxy (shared `inspect/` package). Flagged writes are buffered to a quarantine path and the agent receives `EIO`. (REQ-44)
- Workspace snapshots can be triggered via `POST /sandboxes/{id}/snapshots` for forensic capture and reproducible session replay. (REQ-54)

### 3.6 Audit Bus

- **Kafka** for the audit bus across all deployment modes. Abstracted behind:
  ```go
  type EventBus interface {
      Publish(ctx context.Context, topic string, event Event) error
      Subscribe(ctx context.Context, topic string, opts SubOpts) (<-chan Event, error)
  }
  ```
- Topics: `audit.network`, `audit.filesystem`, `audit.process`, `audit.api`, `audit.policy`. All events share the schema in §9.1 (timestamp, tenant ID, sandbox ID, actor, type, payload). (REQ-39)
- A **persister** consumer writes events into S3/GCS object storage (Parquet, daily roll-up; retention configurable per-tenant). Bodies (request/response, file contents) are stored alongside event metadata for forensic review. PostgreSQL holds only the system-of-record control-plane state — audit events do not live in PG. (REQ-40, REQ-63, REQ-70)
- Tamper evidence: each event carries `prev_hash` chained per-sandbox; the persister verifies and rejects breaks. Daily root hashes are signed with a platform Ed25519 key and published to a transparency log. (REQ-41)

### 3.7 Policy & Secrets Service (`cmd/policysvc`)

- Stores reusable sandbox profiles, RBAC roles, and tenant quotas. Backed by PostgreSQL. (REQ-11, REQ-15)
- Tenant-specific profiles overlay the platform-global ones — a tenant can tighten or override defaults for its own sandboxes without touching the global rows. (REQ-71)
- Acts as the credential-reference resolver: when the API receives a sandbox spec, it asks `policysvc` to resolve each credential reference (`vault://...`, `awskms://...`, `gcpkms://...`) into a short-lived secret bundle, which is pushed directly to the new sandbox's MITM proxy. The bundle never traverses the API server. (REQ-42, REQ-43)

## 4. Data Model (PostgreSQL)

PostgreSQL 15+ is the system of record for all control-plane state. Audit events do not live in PostgreSQL; they go straight from the bus to object storage via the persister.

```sql
-- Tenancy
CREATE TABLE tenants (
    id              UUID PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    quotas          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE actors (
    id              UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    kind            TEXT NOT NULL CHECK (kind IN ('user','service','apikey')),
    external_id     TEXT NOT NULL, -- OIDC subject or cert fingerprint
    roles           TEXT[] NOT NULL DEFAULT '{}',
    UNIQUE (tenant_id, kind, external_id)
);

-- Profiles
CREATE TABLE sandbox_profiles (
    id              UUID PRIMARY KEY,
    tenant_id       UUID REFERENCES tenants(id), -- NULL = platform-global
    name            TEXT NOT NULL,
    spec            JSONB NOT NULL,
    version         INT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name, version)
);

-- Sandboxes (the core entity)
CREATE TABLE sandboxes (
    id                UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL REFERENCES tenants(id),
    actor_id          UUID NOT NULL REFERENCES actors(id),
    spec              JSONB NOT NULL,            -- resolved spec
    spec_hash         BYTEA NOT NULL,            -- SHA256 over canonical spec
    desired_state     TEXT NOT NULL CHECK (desired_state IN ('running','deleted')),
    actual_state      TEXT NOT NULL DEFAULT 'pending',
    state_reason      TEXT,
    node_id           TEXT,                      -- assigned by scheduler
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at        TIMESTAMPTZ,
    terminated_at     TIMESTAMPTZ
);
CREATE INDEX sandboxes_tenant_state_idx ON sandboxes (tenant_id, actual_state);
CREATE INDEX sandboxes_reconcile_idx ON sandboxes (actual_state) WHERE actual_state IN ('pending','starting','draining');

-- Idempotency
CREATE TABLE idempotency_keys (
    key_hash          BYTEA PRIMARY KEY,
    tenant_id         UUID NOT NULL,
    response_code     INT NOT NULL,
    response_body     BYTEA NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Background job purges rows > 24h old.

-- Credential references (no secret values stored here)
CREATE TABLE credential_bindings (
    id                UUID PRIMARY KEY,
    sandbox_id        UUID NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    destination_glob  TEXT NOT NULL,             -- e.g. "api.github.com/*"
    header_name       TEXT NOT NULL,             -- e.g. "Authorization"
    reference_uri     TEXT NOT NULL,             -- e.g. "vault://kv/data/gh-token#token"
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- (Audit events are persisted directly to object storage by the persister consumer;
--  no audit table lives in PostgreSQL.)

-- Filesystem snapshots (metadata; bytes live in object storage)
CREATE TABLE workspace_snapshots (
    id                UUID PRIMARY KEY,
    sandbox_id        UUID NOT NULL REFERENCES sandboxes(id),
    bucket            TEXT NOT NULL,
    object_key        TEXT NOT NULL,
    size_bytes        BIGINT NOT NULL,
    sha256            BYTEA NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-tenant quotas / counters (incremented atomically)
CREATE TABLE tenant_usage (
    tenant_id         UUID PRIMARY KEY REFERENCES tenants(id),
    active_sandboxes  INT NOT NULL DEFAULT 0,
    cpu_cores         NUMERIC NOT NULL DEFAULT 0,
    memory_mb         BIGINT NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Multi-tenancy isolation in PostgreSQL

- All queries that touch tenant data go through a `WithTenant(ctx, tenantID)` repository helper that injects `tenant_id = $1` into every WHERE clause. (REQ-69)
- We additionally enable PostgreSQL **Row-Level Security** on tenant-bearing tables and run app sessions under a role whose `current_setting('app.tenant_id')` is set per-request — defense in depth in case a query is missed. (REQ-69)

## 5. Sandbox Spec Schema

Specs are versioned (`apiVersion: sandbox.platform/v1`) — the canonical declarative input that drives sandbox creation. Canonical form is Protobuf; JSON is the wire format for HTTP. The `security` block (REQ-3), `filesystem` block (REQ-4), and `egress` block (REQ-5) are mandatory; `profile` references a server-side reusable profile (REQ-15). Example (JSON): (REQ-2)

```json
{
  "apiVersion": "sandbox.platform/v1",
  "profile": "code-review-strict",
  "image": "registry.local/agents/coderev:1.4",
  "isolation": "runc",
  "resources": {
    "cpu": "1",
    "memory": "2Gi",
    "disk": "10Gi",
    "timeout": "30m"
  },
  "security": {
    "user": "10001:10001",
    "readOnlyRoot": true,
    "capabilities": { "drop": ["ALL"] },
    "seccomp": "platform/default-strict.json"
  },
  "env": [{ "name": "AGENT_TASK", "value": "review the diff in /workspace" }],
  "filesystem": {
    "backend": "local",
    "workspace": "/workspace",
    "acls": [
      { "path": "/workspace/**", "access": "rw" },
      { "path": "/etc/**", "access": "ro" },
      { "path": "/home/**", "access": "deny" }
    ],
    "quotas": { "bytes": "5Gi", "inodes": 100000, "maxFile": "100Mi" },
    "cow": true,
    "persistArtifacts": ["/workspace/out/**"]
  },
  "egress": {
    "allow": [
      {
        "host": "api.github.com",
        "methods": ["GET", "POST"],
        "paths": ["/repos/*"]
      },
      { "host": "*.pypi.org", "methods": ["GET"] }
    ],
    "rateLimits": [
      { "match": { "host": "api.github.com" }, "rps": 10, "burst": 20 }
    ],
    "credentialBindings": [
      {
        "match": { "host": "api.github.com" },
        "header": "Authorization",
        "valueRef": "vault://kv/data/gh-tokens#bearer"
      }
    ],
    "uploadMaxBytes": "5Mi",
    "contentTypes": {
      "deny": ["application/x-executable", "application/x-mach-binary"]
    }
  }
}
```

The same shape in Go (canonical JSON tags; resource quantities kept as strings so the k8s-style suffixes round-trip — parse them once during validation):

```go
package sandboxv1

// Spec is the top-level sandbox specification (apiVersion: sandbox.platform/v1).
// Canonical form is Protobuf; this struct is the JSON wire shape.
type Spec struct {
	APIVersion string    `json:"apiVersion"`        // e.g. "sandbox.platform/v1"
	Profile    string    `json:"profile,omitempty"` // Named profile to merge under inline overrides.
	Image      string    `json:"image"`             // OCI image reference for the agent container.
	Isolation  Isolation `json:"isolation"`         // Backend selector. v1 accepts only "runc"; reserved for future backends (§18).

	Resources  Resources  `json:"resources"`
	Security   Security   `json:"security"`
	Env        []EnvVar   `json:"env,omitempty"` // Env vars set on the agent process at start.
	Filesystem Filesystem `json:"filesystem"`
	Egress     Egress     `json:"egress"`
}

// EnvVar is a non-secret env var. For secrets, use egress.credentialBindings
// (header injection by the proxy) — never put secret material in plaintext here.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Isolation selects the runtime backend. See cmd/sandboxd Runtime implementations.
type Isolation string

const (
	IsolationRunc Isolation = "runc"
	// gvisor / kata / firecracker are reserved values; not implemented in v1 (§18).
)

// Resources are k8s-style quantity strings ("1", "2Gi", "30m"); validated server-side
// against tenant quota.
type Resources struct {
	CPU     string `json:"cpu"`     // CPU cores or millicores ("1", "500m").
	Memory  string `json:"memory"`  // Memory quantity ("2Gi").
	Disk    string `json:"disk"`    // Workspace upper-layer size ("10Gi").
	Timeout string `json:"timeout"` // Wall-clock TTL as Go duration ("30m").
}

// Security covers the agent process's POSIX/Linux hardening posture.
type Security struct {
	User         string       `json:"user"`         // "uid:gid" the agent runs as; never 0.
	ReadOnlyRoot bool         `json:"readOnlyRoot"` // Mount `/` read-only inside the container.
	Capabilities Capabilities `json:"capabilities"`
	Seccomp      string       `json:"seccomp"` // Profile path/name shipped with the platform.
}

// Capabilities controls Linux capability bounding. Drop precedes Add.
type Capabilities struct {
	Drop []string `json:"drop,omitempty"` // Use ["ALL"] for deny-by-default.
	Add  []string `json:"add,omitempty"`  // Re-add only what the workload truly needs.
}

// Filesystem configures the FUSE-mediated workspace.
type Filesystem struct {
	Backend          FSBackend `json:"backend"`                    // local|s3|gcs|encrypted
	Workspace        string    `json:"workspace"`                  // Mount point inside the agent container.
	ACLs             []ACLRule `json:"acls"`                       // Longest-prefix-match; denied paths return ENOENT.
	Quotas           Quotas    `json:"quotas"`
	COW              bool      `json:"cow"`                        // Copy-on-write upper layer on tmpfs.
	PersistArtifacts []string  `json:"persistArtifacts,omitempty"` // Globs uploaded to backend on teardown.
}

type FSBackend string

const (
	FSBackendLocal     FSBackend = "local"
	FSBackendS3        FSBackend = "s3"
	FSBackendGCS       FSBackend = "gcs"
	FSBackendEncrypted FSBackend = "encrypted"
)

// ACLRule grants Access on paths matching Path (supports `**` globs).
type ACLRule struct {
	Path   string    `json:"path"`
	Access ACLAccess `json:"access"`
}

type ACLAccess string

const (
	ACLAccessRW   ACLAccess = "rw"
	ACLAccessRO   ACLAccess = "ro"
	ACLAccessDeny ACLAccess = "deny"
)

// Quotas bound the workspace's COW upper layer.
type Quotas struct {
	Bytes   string `json:"bytes"`   // Total byte cap ("5Gi").
	Inodes  int64  `json:"inodes"`  // Total inode cap.
	MaxFile string `json:"maxFile"` // Per-file size cap ("100Mi").
}

// Egress configures the per-sandbox MITM proxy that owns the sandbox's only route out.
type Egress struct {
	TLSIntercept       bool                `json:"tlsIntercept"`                 // If false, only host+bytes are logged for matched dests.
	Allow              []EgressAllow       `json:"allow"`                        // Allowlist; everything else is 403'd.
	RateLimits         []RateLimit         `json:"rateLimits,omitempty"`
	InboundAuthHeaders InboundAuthPolicy   `json:"inboundAuthHeaders"`           // Handling of agent-supplied auth headers; see below.
	CredentialBindings []CredentialBinding `json:"credentialBindings,omitempty"`
	UploadMaxBytes     string              `json:"uploadMaxBytes,omitempty"`     // Per-request body cap ("5Mi").
	ContentTypes       *ContentTypePolicy  `json:"contentTypes,omitempty"`
}

// InboundAuthPolicy controls what the proxy does with Authorization/auth-style
// headers the workload set on outbound requests. "strip" forces all auth to come
// from CredentialBindings (the broker), preserving audit-trail integrity.
type InboundAuthPolicy string

const (
	InboundAuthStrip       InboundAuthPolicy = "strip"
	InboundAuthPassthrough InboundAuthPolicy = "passthrough"
	InboundAuthReplace     InboundAuthPolicy = "replace" // Strip only when a binding matches.
)

// EgressAllow is a coarse host+method+path allowlist entry. Host supports `*` wildcards.
type EgressAllow struct {
	Host    string   `json:"host"`              // e.g. "api.github.com" or "*.pypi.org"
	Methods []string `json:"methods,omitempty"` // Default: any.
	Paths   []string `json:"paths,omitempty"`   // Glob patterns; default: any.
}

// RateLimit is a leaky-bucket scoped to (sandbox, match).
type RateLimit struct {
	Match HostMatch `json:"match"`
	RPS   int       `json:"rps"`
	Burst int       `json:"burst"`
}

// HostMatch is the shared selector used by RateLimit and CredentialBinding.
type HostMatch struct {
	Host string `json:"host"` // Exact or wildcard.
}

// CredentialBinding tells the proxy to inject Header with the value resolved from
// ValueRef whenever an outbound request matches Match. Values are pushed to the
// proxy keyring out-of-band; never stored in PG, never logged in cleartext.
type CredentialBinding struct {
	Match    HostMatch `json:"match"`
	Header   string    `json:"header"`   // Typically "Authorization".
	ValueRef string    `json:"valueRef"` // e.g. "vault://kv/data/gh-tokens#bearer"
}

// ContentTypePolicy gates request bodies by Content-Type. Deny precedes Allow.
type ContentTypePolicy struct {
	Deny  []string `json:"deny,omitempty"`
	Allow []string `json:"allow,omitempty"`
}
```

## 6. Control-Plane API Surface

All endpoints live under `/v1/`. The same surface is exposed as gRPC services in the `sandbox.v1` package generated from the same protos. (REQ-1)

| Method                | Path                          | Description                            | REQ           |
| --------------------- | ----------------------------- | -------------------------------------- | ------------- |
| POST                  | `/sandboxes`                  | Create a sandbox.                      | REQ-2, REQ-6  |
| GET                   | `/sandboxes/{id}`             | Get current state and resolved spec.   | REQ-6         |
| PATCH                 | `/sandboxes/{id}`             | Tighten policy (loosen → 409).         | REQ-7         |
| DELETE                | `/sandboxes/{id}`             | Terminate and tear down.               | REQ-8         |
| GET                   | `/sandboxes/{id}/logs`        | Filtered logs; `?follow=true` streams. | REQ-9, REQ-65 |
| POST                  | `/sandboxes/{id}/exec`        | One-shot command.                      | REQ-10        |
| POST                  | `/sandboxes/{id}/files`       | Upload to the workspace (multipart).   | REQ-47        |
| GET                   | `/sandboxes/{id}/files/*path` | Download from the workspace.           | REQ-47        |
| GET                   | `/sandboxes/{id}/snapshots`   | List forensic snapshots.               | REQ-54        |
| POST                  | `/sandboxes/{id}/snapshots`   | Trigger a snapshot now.                | REQ-54        |
| GET/POST/PATCH/DELETE | `/profiles`                   | Manage reusable profiles.              | REQ-15        |

Every mutating endpoint accepts an `Idempotency-Key` header. (REQ-6) `?dryRun=true` returns the _resolved_ spec and policy decision without side effects. (REQ-16)

## 7. Network Path

### 7.1 Egress wiring

On Linux:

- The sandbox container is placed in its own network namespace with no default route except a single rule sending TCP traffic on ports 80/443 to the proxy, and _all other traffic dropped_ (`iptables -A OUTPUT -j DROP`). (REQ-23, REQ-24)
- DNS is forced through the proxy's resolver — no other DNS path is reachable from the netns, eliminating DNS exfiltration. (REQ-28)
- A dedicated `nftables` ruleset on the host blocks `169.254.169.254/32` and `fd00:ec2::254/128` regardless of container policy. (REQ-30)
- IPv6 is disabled per-namespace unless the proxy advertises IPv6 too. (REQ-29)

On macOS (developer mode):

- Sandboxes run in a Linux VM (Lima or OrbStack); the same iptables wiring applies inside the VM. (REQ-57)

### 7.2 TLS interception

- Per-sandbox CA generated at sandbox creation by `policysvc`. Public cert pushed into the sandbox image's trust bundle (`/etc/ssl/certs/sbx-ca.pem` + `update-ca-certificates`); private key passed only to the proxy via a memfd. (REQ-25, REQ-26, REQ-27)
- For non-HTTP TLS or pinned clients (e.g. some Go binaries that refuse system roots), the spec can disable `tlsIntercept` for specific destinations — but at the cost of inspection (we still log host + bytes, just not headers/body).

### 7.3 Policy decision flow

```
agent → connect → proxy intercept → resolve dest → OPA decide → allow?
                                                                  │
                                                    yes ─────────┘├── inject creds (if binding)
                                                                  ├── stream body through inspectors
                                                                  └── forward to upstream, audit
                                                    no ────────────── return 403, audit deny
```

The "decide" step uses the per-sandbox egress allowlist. (REQ-31) "Inject creds" pulls from the proxy keyring populated by the credential broker. (REQ-35) Both `allow` and `deny` outcomes emit audit events. (REQ-39)

## 8. Filesystem Path

### 8.1 Mount lifecycle

1. `sandboxd` calls the FUSE daemon's `Mount(spec)` RPC. (REQ-48)
2. Daemon creates the upper (COW) layer on tmpfs sized to the quota; lower layer is the configured backend. (REQ-51, REQ-53)
3. Daemon mounts at `/var/lib/sbx/<sandbox-id>/workspace`.
4. `sandboxd` bind-mounts that path into the agent container at `spec.filesystem.workspace` — only the spec-declared workspace is exposed. (REQ-45)
5. On teardown: optional snapshot → unmount → destroy upper layer. (REQ-19, REQ-54)

### 8.2 ACL evaluation

- Spec ACLs compile to a longest-prefix-match trie. Lookups are O(path-depth). Sensitive host paths (`~/.ssh`, `~/.aws`, etc.) are deny-listed and return `ENOENT`. (REQ-46, REQ-49)
- Decision cache keyed by `(path, op)` with a small LRU; invalidated on PATCH.
- Audit emission: every `read`, `write`, `unlink`, `rename`, `chmod`, `setxattr` op produces an event. **Sampling is off by default** — every op is recorded and contributes to the per-sandbox `prev_hash` chain, so the replay UI is always complete. Tenants who need to cap audit volume can opt into per-sandbox sampling of high-frequency reads; sampled events skip the chain and the replay UI marks the affected windows as gappy. Denies and writes are never sampled. (REQ-50)

### 8.3 Cross-platform abstraction

A single `fs.Mounter` interface has two implementations:

- `mounter/linux_fuse3.go` (libfuse3 via cgo or pure-Go bazil/fuse)
- `mounter/k8s_csi.go` (talks to the platform's CSI driver and asks for a `bidirectional` propagated mount, so the agent container sees the mediated mount without needing `SYS_ADMIN`) (REQ-56)

There is no macOS-host mounter. On developer Macs the FUSE daemon runs inside the Linux dev VM and uses `linux_fuse3.go`; the macOS host never sees a kernel mount. We don't ship macFUSE because installing the kext requires Recovery-mode "Reduced Security" (too much developer friction); we don't ship FUSE-T because NFSv4-loopback's silly-rename, partial xattrs, and weaker mmap semantics would skew audit and break common tools. Build tags select the implementation; the rest of the daemon (ACL, COW, audit, scanning) is platform-agnostic. (REQ-55)

## 9. Audit & Replay

### 9.1 Event schema

A single shared schema for every event source — network, filesystem, process, API, policy — keyed by tenant and sandbox so a complete session timeline can be reconstructed. (REQ-39)

```go
type Event struct {
    ID         uint64    // assigned by persister
    OccurredAt time.Time // monotonic + wall, set at source
    TenantID   uuid.UUID
    SandboxID  uuid.UUID
    ActorID    uuid.UUID
    Type       string    // network|filesystem|process|api|policy
    Payload    json.RawMessage
    PrevHash   []byte    // SHA256 of previous event's SelfHash, per-sandbox chain
    SelfHash   []byte    // SHA256(prev_hash || canonical(payload) || occurred_at)
}
```

The `PrevHash` chain makes the per-sandbox event log tamper-evident — any break is detected by the persister and surfaced as a verification failure. (REQ-41) Every API mutation (create/PATCH/DELETE/exec) produces an `api`-typed event capturing the actor, spec hash, and decision. (REQ-12)

### 9.2 Pipeline

```
 source → bus (Kafka) → persister → object storage (Parquet, daily roll-up)
```

The persister stores full request/response bodies and file contents alongside event metadata, subject to the per-tenant retention policy. (REQ-40)

### 9.3 Replay UI

A separate read-only service (`cmd/replay`) serves a web UI that reconstructs a session timeline from the audit events: HTTP transactions, file ops, exec calls, and API mutations interleaved by `occurred_at`. Events and bodies are fetched lazily from object storage with HMAC-authorized URLs. The same backing data feeds the live action stream consumed by interactive clients. (REQ-65)

## 10. Secrets & Credential Brokering

The spec carries only credential _references_, not values — the agent process never sees raw secrets. (REQ-35, REQ-42)

```
Client spec includes:
  egress.credentialBindings[i].valueRef = "vault://kv/data/gh-tokens#bearer"   (REQ-5)

API server:
  validates ref format + RBAC (this principal may use this ref?)
  inserts row into credential_bindings (no value)

Scheduler/runtime, on sandbox start:
  policysvc.ResolveBindings(sandboxID) → returns short-lived bundle
  bundle pushed via gRPC (mTLS) → sbxproxy keyring
  proxy holds in memory only

On request:
  proxy matches dest → looks up binding → injects header → forwards
  audit event records the binding ID, never the value      (REQ-37)
```

Credentials never reach the API server response, the agent container, or PostgreSQL. They are scoped per session and revoked by: (REQ-43)

- `DELETE /sandboxes/{id}` → proxy zeroizes its keyring.
- Policy violation → controller signals proxy to revoke a single binding without killing the sandbox.

Outbound bodies pass through the proxy's `secrets` inspector before forwarding (and FUSE writes through the same chain before persistence) — flagged content is quarantined rather than leaked. (REQ-44)

### 10.1 Workload identity

mTLS between the API server, scheduler, `sandboxd`, and the per-sandbox proxy uses **SPIFFE/SPIRE** for short-lived workload identities (1-hour TTL, automatic rotation). The Helm chart ships a SPIRE bundle (server + agent DaemonSet); operators with an existing SPIFFE issuer can swap the bundle out, but SPIRE is the documented default and the only configuration we test against. Revocation is by removing the workload registration in SPIRE — there is no per-cert CRL to maintain.

## 11. Deployment Topologies

The same images, the same policies, and the same audit format apply across every supported mode below. The laptop tier is intentionally narrower (one isolation backend, workspace-inside-VM only) so the developer setup stays predictable and the audit/policy surface is identical to production. (REQ-57)

### 11.1 Local (laptop)

**Supported configuration (the only one):** Apple Silicon (M1 or newer) on macOS 14+ with OrbStack, **or** Linux 5.15+ with cgroup v2. Isolation backend is `runc` — the only one shipped in v1 (§3.3, §18).

- **First step is `bin/sandbox doctor`.** It probes chip / OS version / VM tooling (OrbStack on Mac; KVM + cgroup v2 + LSM on Linux) and prints either `OK: supported` or `unsupported: <reason>`. If it doesn't pass, nothing else starts — there is no degraded-mode path. (REQ-58)
- `docker compose up` brings up: PostgreSQL, Kafka (single-broker KRaft mode), apiserver, scheduler, one `sandboxd`, and an ingress (Caddy) for TLS. **Docker is the laptop-only runtime** — `sandboxd` runs Docker-in-Docker and starts agent containers via the host's Docker socket. Production never uses Docker as the container runtime; it's containerd (§11.2) or CRI-O on OpenShift.
- A `sandbox` CLI (`go install ./cmd/sandbox`) calls the local API.
- On macOS the whole stack runs inside the OrbStack VM. Workspaces live *inside* the VM; bind-mounting host paths (e.g., `~/code`) into a sandbox is unsupported (cross-VM 9p/virtiofs throughput is poor and `inotify` is unreliable). Editor integration uses SSH-Remote / JetBrains Gateway / `code tunnel` against the VM.

### 11.2 Kubernetes

- **Helm chart** under `deploy/helm/sandbox` with subcharts: `apiserver`, `scheduler`, `policysvc`, `audit-pipeline`, `replay`, plus a CRD bundle. (REQ-59)
- `sandboxd` runs as a privileged DaemonSet; sandboxes themselves are unprivileged Pods materialized by a `Sandbox` CRD controller. (REQ-22)
- FUSE on K8s: a CSI driver (`csi.sandbox.platform`) installed via DaemonSet; the FUSE daemon runs as a sidecar with `mountPropagation: Bidirectional` so the mount is visible to the agent container without needing `SYS_ADMIN` on the agent. (REQ-56)
- Distributions: tested on EKS, GKE, AKS, OpenShift, vanilla `kind`. Air-gapped clusters are supported by mirroring all images to a private registry and pinning charts by digest. (REQ-60)
- Kubernetes container runtime is **containerd** (the default since k8s 1.24, when `dockershim` was removed) or **CRI-O** on OpenShift. Docker is **not** a supported k8s CRI runtime — it's laptop-only (§11.1). The agent Pod uses containerd's default `runc` runtime; no `RuntimeClass` is required for the supported configuration. (REQ-61)

### 11.3 On-prem / cloud non-K8s

- Single-binary `sandboxd` + systemd unit for bare-metal. Pluggable runtimes (containerd or Firecracker).
- Terraform modules under `deploy/terraform/{aws,gcp,azure}` for VPC/IAM/buckets/KMS. (REQ-62)
- All persistent state (audit object storage, snapshots, profiles, secrets refs) lives behind pluggable interfaces — no hard dependency on a specific cloud provider's proprietary services. (REQ-63)
- A CI matrix exercises macOS, Linux (amd64 + arm64), and a `kind` Kubernetes cluster end-to-end on every release. (REQ-64)

### 11.4 Supported host environments

One matrix; configurations outside it are unsupported (we don't engage on bug reports for them):

| Concern              | Supported                                                                                                 | Not supported                                                                              |
|----------------------|-----------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------|
| Linux kernel         | 5.15+                                                                                                     | < 5.15                                                                                     |
| Cgroups              | v2 unified                                                                                                | v1, hybrid v1+v2                                                                           |
| LSM                  | AppArmor (Debian/Ubuntu lineage) **or** SELinux (RHEL/Fedora lineage), auto-detected by `sandboxd`        | Hosts with neither, or with custom unprofiled LSMs                                         |
| CNI                  | iptables-mode (kube-proxy) or eBPF (Cilium) with the documented chain-precedence policy applied           | Other CNIs without operator-validated precedence                                           |
| Container runtime    | `runc` (the only v1 isolation backend; §3.3, §18)                                                         | Other OCI low-level runtimes (gvisor, kata, firecracker — future)                          |
| K8s Pod Security     | Baseline cluster-wide; Privileged on `sandboxd` nodes via documented exemption                            | Restricted PSS without an exemption for `sandboxd`                                         |
| macOS local-dev      | Apple Silicon (M1+), macOS 14+, OrbStack, `runc` only                                                     | Intel Macs, macOS < 14, Lima/Colima/Docker Desktop, non-`runc` backends                    |
| Air-gap supply chain | Sigstore Bundle attestations verified offline                                                             | Public Rekor lookup at verify time                                                         |

`sandboxd` preflights every applicable row at startup and refuses to run on a host that fails. `bin/sandbox doctor` runs the same checks for laptop installs.

**Hard limits we don't try to support** (see §17.x for the rationale):

- HTTP/3 / QUIC — UDP is dropped in the sandbox netns; apps that don't fall back to H/2 fail.
- WebSocket per-frame inspection — WS upgrades are allow/deny only.
- Pinned-root TLS clients combined with ECH — no host signal to police.
- Restricted Pod Security Standard for `sandboxd` nodes — operators grant the exemption or scope `sandboxd` to a labeled node pool.

## 12. Observability

- **Metrics**: Prometheus, with stable labels `tenant_id`, `sandbox_id` (low-cardinality alternatives where needed). Key SLIs: API latency p50/p99, sandbox start time, proxy decision latency, FUSE op latency, audit event lag. These metrics also feed the anomaly detector that flags egress-volume spikes, new-domain ratios, and unusual filesystem activity. (REQ-68)
- **Tracing**: OpenTelemetry. Trace context flows from API → scheduler → sandboxd → proxy → upstream so a single agent action can be reconstructed end-to-end.
- **Logs**: structured JSON via `slog` (Go 1.22+). All logs include `tenant_id`, `sandbox_id`, `trace_id`.
- **Live action stream**: clients can `GET /sandboxes/{id}/logs?follow=true` (SSE / gRPC stream) to watch tool calls, file edits, and network requests in real time. (REQ-65)
- **Dashboards**: Grafana JSON shipped under `deploy/dashboards/`.

## 13. Failure Modes & Fail-Closed Semantics

| Component      | Failure           | Behavior                                                                                                                                                                                        | REQ    |
| -------------- | ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------ |
| MITM proxy     | Crash / unhealthy | sandboxd suspends agent (SIGSTOP), tries restart; on second failure terminates sandbox.                                                                                                         | REQ-8  |
| FUSE daemon    | Crash             | Workspace becomes inaccessible (kernel returns EIO); sandbox paused; audit event emitted; daemon restarts and remounts COW upper layer.                                                         | REQ-52 |
| Policy service | Unreachable       | API server rejects new sandbox creates (503); existing sandboxes keep running with their cached policies.                                                                                       |        |
| PostgreSQL     | Read replica down | API server falls back to primary for reads; alerts fire.                                                                                                                                        |        |
| PostgreSQL     | Primary down      | Mutating endpoints return 503; scheduler stops reconciling; running sandboxes continue to serve traffic.                                                                                        |        |
| Audit bus      | Backed up         | Source-side bounded buffer (1 MiB) → spillover to local disk → bus recovery drains. If disk full, sandbox is terminated rather than running un-audited.                                         | REQ-39 |
| Object storage | Down              | Persister buffers to a bounded local spool (per the audit-bus rule above); resumes draining once storage recovers. If the spool fills, sandboxes are terminated rather than running un-audited. |        |

The platform-wide invariant: **no agent action proceeds if it cannot be audited and policy-checked.**

A user-initiated `DELETE /sandboxes/{id}` is the kill switch — it guarantees process termination, FUSE unmount, audit flush, and ephemeral storage destruction. (REQ-8, REQ-66)

## 14. Security Considerations

- **Container escape**: defense in depth on Linux — userns + seccomp + AppArmor or SELinux + dropped capabilities + non-root user + read-only system dirs before the agent runs. The runtime layer in v1 is `runc`, which shares the host kernel; kernel-CVE-grade isolation against hostile workloads requires a future VM-grade backend (§3.3, §18) or a single-tenant deployment. (REQ-20, REQ-21, REQ-22)
- **Proxy bypass**: the only egress path is the proxy, and the agent has no `CAP_NET_RAW`, no `/dev/tcp`, no raw sockets. DNS goes through the proxy. We routinely fuzz with TLS bypass, SNI smuggling, HTTP smuggling, and DNS rebinding payloads. (REQ-23, REQ-24, REQ-28, REQ-30)
- **FUSE bypass**: agent has no path under `/proc` or `/dev` other than what the spec allows; `/proc` is masked with read-only overlays and `/proc/<pid>` from outside the sandbox is filtered. (REQ-46)
- **Side channels**: per-tenant cgroups; we accept the residual risk on shared CPUs and document it. (REQ-69)
- **Supply chain**: all images built reproducibly with SLSA-3 provenance, scanned in CI, signed with `cosign`; the `apiVersion`-pinned spec records the image digest. Verification uses **Sigstore Bundle attestations** embedded in the image — the bundle carries certificate, signature, and inclusion proof, so verifiers don't need to reach Rekor at verify time. Air-gapped clusters get supply-chain verification without standing up an internal Sigstore mirror. (REQ-60, REQ-72)
- **Authn/z fuzzing**: dedicated `cmd/securitytest` exercises the API with malformed tokens, expired certs, role escalation attempts. (REQ-11)

## 15. Cross-Cutting Engineering Conventions

- **Module layout** (`go.mod` is the repo root; one module). The `pkg/sandboxv1/` package holds generated protos consumed by the public Python, TypeScript, and Go client SDKs (built via `buf generate`). (REQ-14)
  ```
  cmd/
    apiserver/  scheduler/  sandboxd/  sbxproxy/  sbxfuse/  policysvc/  replay/  sandbox/
  internal/
    api/        spec/       runtime/    proxy/     fs/         audit/
    auth/       policy/     secrets/    storage/   config/     telemetry/
  pkg/
    sandboxv1/  (generated protos, public)
  deploy/
    helm/       terraform/  compose/    dashboards/
  ```
- **Errors**: typed sentinel errors at package boundaries, `errors.Is`/`As` everywhere; user-facing errors carry a stable `code` for SDK clients.
- **Concurrency**: `context.Context` first arg on every IO call; cancellation deadlines flow through to downstream RPCs; `errgroup` for fan-out.
- **Testing**:
  - Unit tests with table-driven cases.
  - Integration tests (`testcontainers-go` for PostgreSQL/Kafka) on every PR.
  - End-to-end `kind`-based tests on PRs touching deploy.
  - Property tests for the policy evaluator (Rego decisions on randomized requests).
  - Fuzzers for the proxy parsers and FUSE path resolution.

## 16. Migration & Versioning

- API: `/v1` is the contract. Breaking changes go to `/v2` with a one-version overlap.
- Sandbox spec: versioned by `apiVersion`; resolver knows how to upgrade older specs to current.
- Database: migrations under `internal/storage/migrations/` applied with `pressly/goose`. Forward-only.

## 17. Example: running a Claude Agent SDK agent in the sandbox

End-to-end example of a bug-fixing agent built with the [Claude Agent SDK](https://code.claude.com/docs/en/agent-sdk/overview), running unprivileged inside a sandbox. The agent's only egress is `api.anthropic.com` (brokered credentials) and its only writable filesystem is `/workspace`. The example exercises the full spec surface defined in §5 (REQ-2, REQ-3, REQ-4, REQ-5).

### 17.1 Agent code

`fixbug.py` — a minimal agent. The SDK handles the tool loop; we only declare which built-in tools it may use.

```python
import asyncio
import os
from claude_agent_sdk import query, ClaudeAgentOptions, ResultMessage

async def main():
    async for msg in query(
        prompt=os.environ["AGENT_TASK"],
        options=ClaudeAgentOptions(
            allowed_tools=["Read", "Edit", "Bash", "Glob", "Grep"],
            cwd="/workspace",
        ),
    ):
        if isinstance(msg, ResultMessage):
            print(msg.result)

asyncio.run(main())
```

### 17.2 Container image

`Dockerfile` — built once, pushed to a registry the platform can pull from.

```dockerfile
FROM python:3.12-slim
RUN useradd -u 10001 -m agent && pip install --no-cache-dir claude-agent-sdk
USER 10001:10001
WORKDIR /workspace
COPY --chown=10001:10001 fixbug.py /home/agent/fixbug.py
ENV ANTHROPIC_API_KEY=via-broker-do-not-use
CMD ["python", "/home/agent/fixbug.py"]
```

The placeholder `ANTHROPIC_API_KEY` keeps the SDK from short-circuiting at startup; the MITM proxy strips the SDK-attached `x-api-key` header per `inboundAuthHeaders: "strip"` and injects the brokered value (see §17.3).

### 17.3 Sandbox spec

`spec.json` — the resolved spec submitted to the API. Egress is locked to `api.anthropic.com`; the workspace is read-write but `/etc` is read-only and `$HOME` is denied; the API key is brokered from Vault.

```json
{
  "apiVersion": "sandbox.platform/v1",
  "image": "registry.local/agents/fixbug:1.0",
  "isolation": "runc",
  "resources": { "cpu": "1", "memory": "1Gi", "disk": "2Gi", "timeout": "15m" },
  "security": {
    "user": "10001:10001",
    "readOnlyRoot": true,
    "capabilities": { "drop": ["ALL"] },
    "seccomp": "platform/default-strict.json"
  },
  "env": [{ "name": "AGENT_TASK", "value": "Find and fix the bug in auth.py" }],
  "filesystem": {
    "backend": "local",
    "workspace": "/workspace",
    "acls": [
      { "path": "/workspace/**", "access": "rw" },
      { "path": "/etc/**", "access": "ro" },
      { "path": "/home/**", "access": "deny" }
    ],
    "quotas": { "bytes": "2Gi", "inodes": 50000, "maxFile": "50Mi" },
    "cow": true,
    "persistArtifacts": ["/workspace/**"]
  },
  "egress": {
    "tlsIntercept": true,
    "allow": [
      {
        "host": "api.anthropic.com",
        "methods": ["POST"],
        "paths": ["/v1/messages"]
      }
    ],
    "rateLimits": [
      { "match": { "host": "api.anthropic.com" }, "rps": 5, "burst": 10 }
    ],
    "inboundAuthHeaders": "strip",
    "credentialBindings": [
      {
        "match": { "host": "api.anthropic.com" },
        "header": "x-api-key",
        "valueRef": "vault://kv/data/anthropic#api_key"
      }
    ],
    "uploadMaxBytes": "1Mi"
  }
}
```

What this spec gives you:

- **No exfiltration channel**: only `POST https://api.anthropic.com/v1/messages` is reachable; everything else is dropped at the proxy and audited. (REQ-23, REQ-31, REQ-34, REQ-38)
- **No credential leak**: the `ANTHROPIC_API_KEY` env var is a decoy; the real key never enters the container, never appears in the audit log (only an HMAC of the value), and is zeroized when the sandbox is deleted (§10). (REQ-35, REQ-37, REQ-42, REQ-43)
- **Bounded blast radius**: a compromised agent can write only to `/workspace`, can't reach instance metadata (`169.254.169.254` blocked at the host, §7.1), and can't read `/home` or other tenants' data (RLS, §4). (REQ-30, REQ-45, REQ-46, REQ-49, REQ-69)
- **Reproducibility**: workspace COW + artifact persistence on teardown. (REQ-19, REQ-47, REQ-51)
- **Replayable**: every tool invocation, every HTTP request to Anthropic, and every workspace mutation is on the audit bus and reconstructable in the replay UI (§9). (REQ-39, REQ-40, REQ-50)

### 17.4 Submitting the sandbox

```bash
curl -sX POST https://sandbox.platform.local/v1/sandboxes \
  -H "Authorization: Bearer $SANDBOX_TOKEN" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  --data-binary @spec.json
```

Or with the bundled CLI:

```bash
sandbox run --spec spec.json --follow
# → streams stdout/stderr from the agent until exit; on completion,
#   /workspace/** is uploaded to the artifact bucket per persistArtifacts.
```

### 17.5 Variations

- **Read-only review agent**: drop `Edit`/`Bash` from `allowed_tools`, set `acls` to `ro` everywhere. Useful for a `code-reviewer` subagent that should not mutate the repo.
- **Subagent fan-out**: keep `Agent` in `allowed_tools` and define one or more `agents` in `ClaudeAgentOptions`; the same sandbox spec covers all subagents — they share the egress allowlist and credential broker.
- **MCP servers**: add the MCP server's host to `egress.allow` and (if required) a `credentialBindings` entry. The MCP server itself can run in a sibling sandbox or as a sidecar inside the same pod.
- **Bedrock / Vertex / Foundry**: replace the `api.anthropic.com` allow + binding with the cloud provider's endpoint; the SDK reads `CLAUDE_CODE_USE_BEDROCK=1` (etc.) and the same broker pattern applies.

## 18. Open Questions / Future Work

- GPU isolation and metering.
- Multi-region active-active control plane (today: regional active-passive with PostgreSQL streaming replication).
- WASM-based agent runtime as an alternative to containers for trusted, deterministic workloads.
- Differential privacy / k-anonymity in audit replay for compliance teams that must browse without seeing raw bodies.
- End-to-end data-residency knobs (per-tenant region pinning for object storage and PG) — partially supported today via per-tenant retention but not full residency. (REQ-70)
- Built-in human-in-the-loop approval queue for `Per-action approval mode`; today this is a webhook that defers to an external system. (REQ-67)

### Evaluated alternatives: VM-grade isolation backends

v1 ships `runc` only (§3.3). The `Runtime` interface in `cmd/sandboxd` is preserved so additional backends can be added without an architectural change. Summary of the alternatives we considered and why they're future work:

| Backend       | Boundary                              | Cold start  | Memory floor | Host requirement                  | Why not in v1                                                                                  |
|---------------|---------------------------------------|-------------|--------------|-----------------------------------|------------------------------------------------------------------------------------------------|
| `gvisor`      | Userspace kernel (Sentry)             | 100–300 ms  | ~30 MB       | cgroup v2 (KVM optional, faster)  | Most likely second backend. Curated syscall surface (gaps in `io_uring`, BPF, exotic `ioctl`s) needs a known-incompatible workload list and per-image validation we don't want to own in v1. |
| `kata`        | Hardware VM (real Linux guest)        | 1–3 s       | ~80–120 MB   | `/dev/kvm`                        | Strongest isolation but needs nested virt; most managed K8s nodes (default EKS/GKE/AKS) don't expose `/dev/kvm`, so the matrix complication is large for a small audience.                |
| `firecracker` | Hardware microVM (minimal device set) | 125–500 ms  | ~5–20 MB     | `/dev/kvm`                        | Same nested-virt constraint as `kata`, plus a stripped device model that requires per-image compatibility validation.                                                                     |

**When to revisit:** customer demand for "VM-grade isolation against hostile workload code" (compliance scopes, multi-tenant untrusted code on shared nodes). `gvisor` is the most likely first add — process-level, drops in via `containerd-shim-runsc-v1`, no nested-virt needed. `kata`/`firecracker` would follow only if a single-tenant + bare-metal customer paid for them.

**What needs to happen to add `gvisor`** (illustrative, not committed):

1. Allow-list `"gvisor"` in the `spec.isolation` validator and the `Isolation` Go enum (§3.3, §5).
2. Ship a `RuntimeClass: gvisor` in the Helm chart with the `runsc` handler, gated on a node label.
3. Preflight in `sandboxd`: detect the `runsc` shim binary on the node when the spec asks for `gvisor`.
4. Maintain a "known-incompatible workloads" list (`io_uring`, BPF syscalls, exotic `ioctl`s) and a CI suite that exercises the published agent images against `runsc` on every release.
5. Update §11.4 to add a "Container runtime" supported value of `gvisor` and §3.3's Isolation backend section to make the trade-off explicit.

For customers who need VM-grade isolation today: run on a single-tenant cluster, or use a hosted offering with VM-grade isolation built in (GKE Sandbox, AWS Fargate, Azure Container Apps with isolated SKUs).

### macOS local-dev limitations

§11.1 currently claims "feature parity with the production deployment except for scale." That overstates the macOS story. Concrete gaps for someone running the stack on a Mac via OrbStack/Lima:

- **Isolation backend reliability on Mac was a major input to v1 scoping.** Only `runc` is reliable on every Mac. `gvisor` falls back to slow `ptrace` mode without nested virt; `kata` and `firecracker` need nested KVM, which Apple's `Hypervisor.framework` only exposes on M3+ silicon on macOS 15+. Rather than ship a backend matrix the laptop can't actually exercise, v1 ships `runc` only (§3.3). (REQ-17, REQ-57, REQ-61, REQ-64)
- **arm64 image coverage.** On Apple Silicon, amd64 agent images run under `qemu-user-static` at 5–10× slowdown and break subtle syscall semantics — which matters because the platform fuzzes seccomp, TLS bypass, SNI smuggling, and DNS rebinding (§14). Either the CI matrix publishes arm64 variants of every shipped image, or local fuzz results diverge from CI. (REQ-64)
- **macFUSE on the host is a nonstarter.** The `mounter/darwin_macfuse.go` path requires a kernel extension whose installation on Apple Silicon needs Recovery-mode "reduced security" — too much friction to mandate. Practically, FUSE on macOS only works *inside* the Linux VM, making the macOS-host mounter dead code. We should either delete it or replace with FUSE-T (NFSv4-loopback) and accept the behavior delta.
- **Cross-boundary workspace I/O.** Bind-mounting a macOS source tree into the sandbox workspace traverses macOS → virtio-fs/9p → Linux VM → FUSE → backend. Throughput is poor and `inotify` across the boundary is unreliable. The recommended pattern (workspace lives inside the VM) hurts editor integration on the host.
- **Resource floor in real units.** macOS (~7 GB) + Linux VM (~4–6 GB) + Postgres + Kafka JVM (~1.5 GB) + per-sandbox overhead (~200 MB each) puts the comfortable minimum at **32 GB**. 16 GB Macs can run the stack but only 1–2 concurrent sandboxes before swap. Doc should state this explicitly rather than implying parity.
- **Audit timing oddities.** macOS suspends the VM on lid-close; the sandbox's monotonic-vs-wall clock pairing in audit events (§9.1) drifts until resync. Not security-relevant — `prev_hash` chain still verifies — but reordered events in the replay UI confuse reviewers.

**Action items — favor simplicity; if a configuration is hard to support, refuse it and document the limit:**

1. **One supported local-dev configuration.** Replace §11.1's parity claim with a single supported path: Apple Silicon (M1 or newer), macOS 14+, OrbStack, `isolation: runc` (the only v1 backend, §3.3). Everything outside this is unsupported — no "best effort" tier.
2. **Delete `mounter/darwin_macfuse.go`.** macFUSE requires Recovery-mode "Reduced Security," which is not something we can ask developers to enable. The macOS-host mounter is dead code; on macOS the workspace lives *inside* the Linux VM, period. (Re-evaluate FUSE-T only if a future feature genuinely requires a host-side mount, and accept the silly-rename / xattr / mmap deltas if we do.)
3. **Ship arm64 images for local use; amd64 only in CI.** Don't publish amd64 variants for the laptop quickstart. Running amd64 under QEMU is too slow and diverges on the syscall surfaces we fuzz (§14).
4. **`sandbox doctor` as the first quickstart step.** Single command: detects chip / macOS version / OrbStack and prints either "supported" or "not supported: <reason>." It does not suggest workarounds and does not start anything on an unsupported host.

**Limits we won't try to paper over on macOS:**

- `gvisor`, `kata`, `firecracker` are **out of scope for v1** (§3.3, §18). The spec validator rejects them; they're a documented future-work item, not a preflight failure on a per-host basis.
- **Host-bind-mounted workspaces are unsupported.** Workspace data lives inside the VM; editor integration is via SSH-Remote / JetBrains Gateway / `code tunnel` — not by mounting `/Users/...` into the sandbox.
- **macOS-only filesystem semantics don't survive the boundary.** `com.apple.quarantine`, Spotlight metadata, FSEvents, resource forks: assume gone.
- **Lid-close clock skew** can reorder audit events in the replay UI. The `prev_hash` chain still verifies, but the timeline view will look out of order until the next resync. Documented behavior, not a bug to chase.

### Linux production gotchas

Linux is the primary target so most of the design works as written, but several real gaps and host-environment dependencies are under-specified. Grouped by category:

**Gated by host capability**

- **cgroup v2 is required but not stated.** §3.3 says "cgroups v2"; many enterprise distros still default to v1 (RHEL 8.x, older Ubuntu LTS), and hybrid v1+v2 mode silently splits controllers so resource limits look enforced but aren't. State a minimum kernel + cgroup-version requirement and have sandboxd refuse to start on non-v2 hosts. (REQ-18)
- **PSS Restricted profile blocks the design.** sandboxd needs `privileged: true`, `mountPropagation: Bidirectional`, and `hostPath` mounts for cgroupfs — all forbidden by Kubernetes' Restricted Pod Security Standard, which is increasingly the default in enterprise clusters. Helm chart needs to ship a documented PSS exemption (or matching OPA Gatekeeper / Kyverno policy) for sandboxd-eligible nodes. (REQ-22, REQ-59)

**Under-specified host interactions**

- **CNI / iptables / nftables conflicts.** §7.1 prescribes `iptables -A OUTPUT -j DROP` and `nftables` rules. On clusters running Cilium or Calico-eBPF, our DROP rule may be a no-op for traffic the CNI has fast-pathed; on iptables-mode CNIs, our rules can collide with CNI-owned chains. The design needs an explicit story for "we are not the only thing writing netfilter rules" — e.g., a dedicated chain we own with a documented precedence requirement against common CNIs. (REQ-23, REQ-24)
- **Resource visibility inside the sandbox is wrong without LXCFS.** cgroups enforce CPU / memory limits but the agent process still sees host `/proc/cpuinfo`, `/proc/meminfo`, `/sys/devices/system/cpu`. Java / Go runtimes spin up worker pools sized to the host (e.g., 96 threads on an EPYC node for a 1-CPU sandbox) and Python ML libs allocate buffers based on host free memory. Bind-mount LXCFS or a shim, or document the workaround in §3.3. (REQ-18)
- **AppArmor vs SELinux is a kernel-build choice, not both.** §14 says "userns + seccomp + AppArmor" but RHEL/CentOS/Fedora ship SELinux instead. Helm chart must detect the active LSM and ship the right profile, or the design has to declare seccomp alone as load-bearing. (REQ-21)
- **mTLS cert distribution for per-sandbox proxies is undefined.** §3.4 + §10 push secrets over "the same gRPC + mTLS channel used elsewhere," which implies short-lived per-workload identities — almost certainly SPIFFE/SPIRE, but it isn't named. Pick the identity issuance backend explicitly so cert rotation / revocation has an owner. (REQ-42, REQ-43)

**Security boundaries that need more design**

- **TLS interception has growing blind spots.**
  - **HTTP/3 / QUIC over UDP:** netns drops UDP, so QUIC connections fail. Apps that fall back to H/2 are fine; gRPC-over-QUIC and similar are not.
  - **Pinned-root apps** (Go with `MinVersion`, Java custom truststore, some SDK retry paths): TLS intercept fails closed. §7.2 acknowledges this with "disable `tlsIntercept` for specific destinations" but that's a security regression — bytes through unintercepted, no body inspection. Need a clearer policy on when this is acceptable.
  - **Encrypted Client Hello (ECH):** strips SNI-based hostname filtering. Adoption small but growing; the proxy's host matching needs to look at intercepted plaintext, not SNI.
  - **WebSockets / streaming bidi:** inspectors in §3.4 are designed for request/response. Per-frame WS scanning isn't in the design. (REQ-25, REQ-31, REQ-44)
- **Audit event volume can flood Kafka and create replay gaps.** A `find /workspace` or `pip install` produces tens of thousands of FS events per second. §8.2's "sampled to avoid drowning the bus" creates **forensic replay gaps** — exactly the noisy operations a reviewer would want to see. The design should specify which event classes are sampling-eligible vs always-recorded, and how sampled events interact with the per-sandbox `prev_hash` chain (sampled events can't be in the chain or chain integrity breaks). (REQ-39, REQ-41, REQ-50)
**Supply chain**

- **Air-gapped clusters can't reach public Sigstore/Rekor.** REQ-60 says air-gapped is supported by mirroring images, but §14 also requires `cosign`-signed images with SLSA-3 provenance. By definition, air-gapped clusters can't reach the public transparency log; verification fails unless the operator stands up an internal Sigstore + Rekor stack. Today this is aspirational — needs an explicit "internal Sigstore mirror required" requirement or a documented bundled-attestation alternative. (REQ-60, REQ-72)

**Action items — favor simplicity; if a configuration is hard to support, refuse it and document the limit:**

1. **Single "Supported host" matrix in §11.** Linux 5.15+, cgroup v2 only, one of {AppArmor on Debian/Ubuntu lineage, SELinux on RHEL/Fedora lineage}, iptables-mode or eBPF CNI with the chain precedence we document. Outside this matrix is unsupported — not "may work."
2. **`sandboxd` refuses to start on unsupported hosts.** Hard preflight: cgroup v2 (fail on v1 or hybrid, no opt-out) and active LSM matches the shipped profile. One-line error and exit; no degraded-mode start.
3. **Bundle LXCFS in the sandbox base image and bind-mount by default.** No "or equivalent." Resource visibility is mandatory so runtimes size pools to sandbox limits, not host.
4. **SPIRE is *the* identity backend.** Write it into §10, ship a SPIRE bundle in the Helm chart, drop the hedging language. Operators who already run a different SPIFFE issuer can swap; that's their integration to own.
5. **Audit sampling is opt-in, off by default.** All filesystem and network events go on the per-sandbox `prev_hash` chain. Sampling is a per-tenant tunable for cost-sensitive deployments who explicitly accept replay gaps; the replay UI marks sampled windows. Document the opt-in in §8.2 and §9.1.
6. **Air-gap uses Sigstore Bundle attestations only.** Verifier reads the bundle from the image; no Rekor lookup at verify time. Drop the "mirror public Sigstore internally" plan — too much surface area for too little gain.

**Limits we won't try to support on Linux:**

- **HTTP/3 / QUIC**: UDP is dropped in the sandbox netns. Apps that don't fall back to H/2 fail. We are not adding a UDP-aware proxy path.
- **Pinned-root TLS clients** (Go `MinVersion`-style truststores, custom Java keystores, some SDK retry paths): TLS intercept fails closed. Operators can opt destinations out via `tlsIntercept: false` per host, but that's a deliberate inspection regression they sign for — not a feature to expand.
- **WebSocket per-frame inspection**: not supported. WS upgrades are allow/deny only; the inspector chain (§3.4) does not see frames.
- **VM-grade isolation backends** (`gvisor`, `kata`, `firecracker`): not shipped in v1 (§3.3, §18). The interface in `cmd/sandboxd` is preserved so they can be added later, but the spec validator rejects them today.
- **Kubernetes Restricted PSS**: `sandboxd` requires `privileged: true` + `mountPropagation: Bidirectional` + cgroupfs `hostPath`. Operators grant a PSS exemption (or scope `sandboxd` to a labeled node pool); there is no Restricted-mode variant.
- **CNIs we haven't validated chain precedence against**: explicitly unsupported. The Helm chart documents the required chain order and a Cilium policy snippet; anything else needs operator validation before we'll engage on bug reports.
- **ECH-encrypted SNI**: when ECH is in use, host-allowlist matching falls back to the intercepted plaintext Host header. Pinned + ECH together can't be policed and are unsupported in combination.
