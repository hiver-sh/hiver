# Hiver Threat Model

This document describes the security model of the Hiver agent runtime: what each
component trusts, what it is trusted to enforce, the attacks it is designed to
resist, and where its boundaries end. It covers the three components that make up
a typical deployment:

- **Sandbox** — the per-agent runtime pod (`sandboxd` + `sbxproxy` + `sbxfuse`
  - the isolated workload on `runc` or `firecracker`).
- **Controller** — the host-side control plane that provisions sandbox pods.
- **Gateway** — the Envoy edge proxy that routes external callers to the
  controller and to individual sandboxes.

> Status: living document. It describes the design intent and the current
> implementation. Where the implementation does not yet meet the intended
> control (e.g. transport authentication), that gap is called out explicitly as
> a **Gap**, not glossed over.

---

## 1. Core security thesis

**Everything inside the sandbox is untrusted.** The agent, the model it talks to,
the code it runs, and any data it ingests are all assumed to be potentially
adversarial — whether through a jailbreak, a prompt-injection payload buried in a
fetched web page or a mounted file, a supply-chain compromise in the agent's
dependencies, or a deliberately malicious task.

Security therefore does **not** rely on the agent behaving. The controls that
matter are enforced _outside_ the workload's control boundary, by code the agent
cannot reach or modify:

| Control                                 | Enforced by                       | Trust boundary                                                              |
| --------------------------------------- | --------------------------------- | --------------------------------------------------------------------------- |
| Egress allow/deny + request rewriting   | `sbxproxy` (MITM proxy)           | Outside the workload; the workload's only route off-box is through it       |
| Filesystem path ACLs (`ro`/`rw`/`deny`) | `sbxfuse` (FUSE daemon)           | Every read/write traps to the daemon, which is in the pod, not the workload |
| Credential injection (auth tokens)      | `sbxproxy` egress rule `override` | Secret is bound to the rule, applied after the request leaves the workload  |
| Process / namespace / cgroup isolation  | `runc` or `firecracker`           | Kernel-enforced boundary around the workload                                |
| Audit event stream                      | `sandboxd`                        | Records _attempts_, allowed and denied, that the workload cannot suppress   |

The guiding property: **a fully compromised agent should be able to do exactly
what policy permits and nothing more, and every attempt it makes — successful or
blocked — should be visible in the event stream.**

---

## 2. Components, assets, and trust levels

```
        external caller (SDK / CLI / inspector)
                      │
                      ▼
              ┌───────────────┐
              │    Gateway    │  Envoy edge; path-based routing
              │  (port 10000) │  /controller/* → controller
              └───────┬───────┘  /sandbox/<id>/* → that sandbox
            ┌─────────┴──────────┐
            ▼                    ▼
     ┌─────────────┐     ┌──────────────────────────────────┐
     │ Controller  │     │            Sandbox pod           │
     │ (port 9000) │     │ ┌──────────────────────────────┐ │
     │ docker.sock │     │ │ sandboxd (API :8099)         │ │
     │ provisions ─┼────►│ │  ├─ sbxproxy (egress MITM)   │ │
     │  pods       │     │ │  ├─ sbxfuse  (FS ACLs)       │ │
     └─────────────┘     │ │  └─ workload (runc/firecrkr) │ │
                         │ │       = UNTRUSTED agent      │ │
                         │ └──────────────────────────────┘ │
                         └──────────────────────────────────┘
```

### Trust levels (most → least trusted)

1. **Host / Docker daemon / Kubernetes API** — the root of trust. Compromise here
   is game over for every sandbox on the node. Out of scope to defend _from_ here;
   in scope to _limit what is exposed to it_.
2. **Controller** — trusted to create pods correctly and not to leak the host. It
   holds host-level privilege (the Docker socket or a K8s service account).
3. **Sandbox sidecars** (`sandboxd`, `sbxproxy`, `sbxfuse`) — trusted to enforce
   policy. They run in the pod but _outside_ the workload's isolation boundary.
4. **Gateway** — trusted only to route. It terminates and forwards; it is not a
   policy decision point and holds no secrets.
5. **The workload** (agent + its processes + ingested data) — **fully untrusted.**

### Key assets to protect

- **Injected credentials** — auth tokens / API keys carried in egress rule
  `override`s. The whole point is that the agent never sees them.
- **Read-only / denied data** — org knowledge, secrets, anything mounted `ro` or
  `deny`. Must survive a jailbroken agent.
- **The per-sandbox CA private key** — `sbxproxy` mints leaf certs with it to MITM
  TLS. Theft lets an attacker forge certs the workload trusts.
- **Host control surface** — the Docker socket / K8s credentials held by the
  controller.
- **Tenant isolation** — one sandbox must not reach another's data, network, or
  filesystem.
- **The audit trail** — its integrity and completeness.

---

## 3. Sandbox

The sandbox is the unit of isolation. It is one container ("pod") that runs
`sandboxd` as PID 1, which launches the sidecars and then the untrusted workload
inside a _nested_ isolation boundary (`runc` container or `firecracker` microVM).

### 3.1 What it enforces

**Egress (`sbxproxy`).** All TCP egress from the workload is transparently
redirected to `sbxproxy` and there is no route around it:

- **runc backend:** an `OUTPUT`-chain nat `REDIRECT` rule sends all workload TCP
  to the proxy port; proxy- and FUSE-originated upstream traffic is stamped with
  `SO_MARK` and `RETURN`s early to avoid a redirect loop
  (`internal/isolation/container.go`).
- **microvm backend:** the guest is a separate network stack; its egress arrives
  on the host tap and is `DNAT`'d in `PREROUTING` to `127.0.0.1:proxyPort`
  (`route_localnet` lifts the martian-packet drop), and an in-guest firewall
  mirrors the rules (`internal/isolation/microvm.go`).

The proxy evaluates ordered allow/deny rules (first match wins, **empty list =
deny all**). For HTTPS it can either raw-forward after matching SNI host + port,
or — when configured with the per-sandbox CA — terminate TLS and additionally
match method/path/headers, inject `override` headers and query params, and decode
the body for audit. `Passthrough` rules opt out of interception to preserve the
client's TLS fingerprint for fingerprint-sensitive WAFs.

**Filesystem (`sbxfuse`).** Every mount is FUSE-backed. Each read/write/lookup
traps into the daemon, which evaluates path ACLs **most-specific-first with a
default of deny** (`internal/fusefs/acl.go`). A denied path returns `ENOENT` — the
agent cannot even confirm existence. ACLs are enforced by the daemon, not the
agent, so a jailbroken workload still cannot write a `ro` mount or read a `deny`
path. Every attempt emits an `fs.request` event with its verdict.

**Process isolation.** The workload runs under `runc` (namespaces + cgroups) or
`firecracker` (a KVM microVM with a separate kernel). The rootfs is overlayfs:
the agent's writes layer over a read-only base image.

### 3.2 Threats & mitigations

| Threat                                                     | Mitigation                                                                                                                                     |
| ---------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Agent exfiltrates data to an arbitrary host                | Default-deny egress; only allowlisted hosts reachable; every request audited                                                                   |
| Agent reads an injected auth token                         | Token lives in the proxy `override`, applied _after_ the request leaves the workload; the agent's own headers are overwritten, never read back |
| Agent bypasses the proxy (raw socket, direct IP, alt port) | iptables REDIRECT/DNAT catches _all_ TCP regardless of destination or port; non-TCP has no route out                                           |
| Agent writes to read-only data / reads secrets             | FUSE ACL default-deny, evaluated in the daemon; `deny` returns ENOENT                                                                          |
| Agent tampers with policy by editing config files          | Policy lives in `sandboxd`'s work dir outside the workload's mount namespace; the workload cannot reach it                                     |
| Agent escapes the workload                                 | runc namespace/cgroup confinement, or firecracker hardware (KVM) boundary for a stronger guarantee                                             |
| Agent suppresses its own audit trail                       | Events are emitted by the sidecars, not the workload; it has no handle to the stream                                                           |
| Stolen CA outlives the workload                            | CA is per-sandbox, generated fresh per `sandboxd` start, 7-day validity, never leaves the pod (`internal/proxy/ca.go`)                         |

### 3.3 Residual risks & boundaries

- **MITM CA in the trust store.** Interception requires the orchestrator to
  install the per-sandbox CA into the workload's trust store. Any process in the
  workload then trusts that CA — fine within the throwaway sandbox, but it means
  the workload _can_ be MITM'd by its own proxy by design. The CA key is the asset
  to protect; it stays in the pod and is short-lived.
- **runc vs firecracker.** `runc` shares the host kernel; a kernel-level container
  escape is the relevant tail risk and `firecracker` exists precisely for
  workloads where that risk is unacceptable. Choose the backend to match the
  threat.
- **Sidecar privilege.** `sandboxd` and the sidecars run _in_ the pod but outside
  the workload. A bug in `sbxproxy`/`sbxfuse`/`sandboxd` reachable from workload
  input (a malicious HTTP response parsed by the proxy decoders, a crafted path
  through FUSE) is a privilege-escalation surface — these parsers are the highest-
  value code to fuzz and review.
- **Covert channels.** Resource-usage and timing side channels (and DNS-style
  encoding within _allowed_ hosts) are not eliminated; the audit stream surfaces
  volume but policy is host-granular, not content-granular.
- **DoS within the pod.** cgroup limits (`VcpuCount`/`MemoryMiB`) bound resource
  use; a workload can still exhaust its own quota. Cross-tenant DoS depends on the
  host's pod-level limits.

---

## 4. Controller

The controller is the host-side control plane (`cmd/controller`,
`internal/api/controller`). Its only job: given a caller-chosen `key` and a
`SandboxConfig`, idempotently provision a sandbox pod and return how to reach it.
It supports a Docker runtime (shells out to the `docker` CLI against the host
daemon) and a Kubernetes runtime.

### 4.1 Privilege held

This is the **most privileged** Hiver-authored component. In the Docker runtime it
has the **host Docker socket mounted read-write** (`docker/compose.yaml`) and
creates sandbox containers with a notably broad profile
(`internal/api/controller/docker_runtime.go`):

- `--cap-add SYS_ADMIN, NET_ADMIN, DAC_READ_SEARCH`
- `--security-opt apparmor=unconfined`, `--security-opt seccomp=unconfined`
- `--device /dev/fuse` (+ `/dev/kvm`, `/dev/net/tun`, loop devices for microvm)
- `--cgroupns host` with `/sys/fs/cgroup` bind-mounted read-write

These are required so the pod can set up the _inner_ isolation (mount overlayfs +
FUSE, create the firecracker tap, install iptables). The trade-off is explicit:
the **pod boundary is weak by design — the real isolation is the nested
runc/firecracker boundary inside it**, not the outer container. The outer
container is a privileged host process; treat it as such.

### 4.2 Threats & mitigations

| Threat                                                      | Mitigation / status                                                                                                       |
| ----------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| Access to the Docker socket = host root                     | Accepted: controller is trusted host-side infra. Isolate it; do not co-locate untrusted tenants with it                   |
| Concurrent create races on the same key                     | `GetOrCreateSandbox` serializes lifecycle ops under a mutex; idempotent on key, so racing callers converge on one sandbox |
| `key` injection into container names / docker args          | `key` is constrained to `^[A-Za-z0-9_-]{1,64}$` by the OpenAPI schema; names are derived, not interpolated from free text |
| Malicious `SandboxConfig` (e.g. arbitrary host bind-mounts) | `NormalizeConfig` processes config; **local `Origin` mounts are bind-mounted from host paths** — see Gap below            |
| Unauthenticated provisioning                                | **Gap** — see 4.3                                                                                                         |

### 4.3 Gaps & residual risks

- **No authentication or authorization on the controller API.** `NewControllerServer`
  binds `0.0.0.0:9000` with no auth middleware (`controller_server.go`). Anyone who
  can reach the port can create, list, or destroy any sandbox, and can stream all
  sandboxes' lifecycle events. **The controller must never be exposed beyond a
  trusted network boundary**; today the gateway is the only thing in front of it
  and the gateway does not authenticate either. AuthN/Z is the top hardening item.
- **Host bind-mounts from config.** A `local` filesystem with an `Origin` is
  bind-mounted from a host path into the pod
  (`-v origin:mount`). A caller who can submit a `SandboxConfig` can therefore ask
  the controller to bind arbitrary host paths. Combined with the no-auth gap, the
  origin allowlisting / caller trust must be enforced upstream of the controller.
- **Tenant `key` namespace is flat and global.** Keys are not scoped per caller;
  any caller who knows or guesses a key gets that sandbox (`GetOrCreate` returns
  the existing one). Treat keys as capabilities and keep the API private.
- **Spec written to a predictable temp path.** `Start` writes
  `${TMPDIR}/hive-spec-<key>.yaml` (mode 0644) then `docker cp`s it in. On a shared
  host this is a readable-config / symlink-race surface; mitigated in practice by
  the controller running alone in its own container.

---

## 5. Gateway

The gateway is an Envoy edge proxy (`docker/gateway`, `envoy.yaml`) on port 10000.
It is the single externally-published port of the stack and does **path-based
routing only**:

- `/controller/*` → the controller cluster (`prefix_rewrite: /`).
- `/sandbox/<id>/*` → a Lua filter rewrites `:authority` to
  `hiver-sandbox-<id>:8099` and a dynamic-forward-proxy resolves that DNS name and
  forwards the request to that sandbox's API.

### 5.1 Role in the model

The gateway is a **router, not a policy point.** It holds no secrets, makes no
authorization decision, and terminates no agent traffic. Its security value is
narrow: it is the choke point that _could_ host edge auth, rate limiting, and TLS
termination — but as configured it does none of those.

### 5.2 Threats & gaps

| Threat                                               | Status                                                                                                                                                                                                                                                                          |
| ---------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Unauthenticated access to controller + every sandbox | **Gap** — `domains: ["*"]`, no auth filter; anyone reaching :10000 can hit `/controller/*` and any `/sandbox/<id>/*`                                                                                                                                                            |
| Sandbox ID enumeration / cross-tenant access         | The `<id>` from the path is templated directly into the upstream authority via Lua; knowing an ID is sufficient to reach that sandbox's full API (exec, file read/write, config). IDs are UUIDs, so unguessable, but they are **bearer identifiers with no accompanying authN** |
| Plaintext transport                                  | No TLS listener configured; traffic to and through the gateway is cleartext unless terminated upstream                                                                                                                                                                          |
| SSRF via dynamic forward proxy                       | The forward-proxy cluster will resolve and connect to whatever `:authority` the Lua sets; the Lua constrains it to the `hiver-sandbox-<id>` pattern, but the regex `^/sandbox/([^/?#]+)` is the only validation of `<id>` — keep that pattern strict                            |
| No rate limiting / request caps                      | A caller can spam create/exec; bound this upstream                                                                                                                                                                                                                              |

### 5.3 Hardening direction

The gateway is the natural home for the controls the stack currently lacks:
terminate TLS, require an auth token (mTLS or bearer) on `/controller/*` and
`/sandbox/*`, bind a sandbox ID to the credential that created it so possessing an
ID is not enough, and add rate limits. Until then, **the entire stack assumes the
gateway sits inside a trusted network perimeter.**

---

## 6. Cross-cutting: trust boundaries summary

| Boundary                    | Crossing mechanism                               | What enforces it                                      |
| --------------------------- | ------------------------------------------------ | ----------------------------------------------------- |
| External caller → stack     | Gateway :10000                                   | Routing only — **no authN today (Gap)**               |
| Caller → host privilege     | Controller → Docker socket / K8s                 | Controller is trusted infra; must be network-isolated |
| Sandbox ↔ sandbox           | Separate pods, separate networks, UUID-addressed | Per-pod isolation + Docker/K8s network separation     |
| Workload → network          | iptables REDIRECT/DNAT → `sbxproxy`              | Inescapable proxy interception; default-deny rules    |
| Workload → filesystem       | FUSE trap → `sbxfuse`                            | Path ACLs, default-deny, daemon-enforced              |
| Workload → injected secrets | Proxy `override` applied post-egress             | Secret never enters the workload's address space      |
| Workload → host kernel      | runc namespaces / firecracker KVM                | Kernel / hypervisor boundary                          |

---

## 7. Top priorities (summary)

1. **Authentication & authorization** on the controller and gateway. This is the
   single largest gap: today anything that reaches the gateway can provision,
   inspect, and execute in any sandbox. The whole stack currently depends on an
   external trusted-network assumption.
2. **Bind sandbox identity to a credential** so a leaked/guessed sandbox ID is not
   a full capability over that sandbox's exec/file/config API.
3. **TLS everywhere** at the gateway and between components.
4. **Constrain `SandboxConfig`** accepted by the controller — especially `local`
   `Origin` host bind-mounts and `Image` — behind an allowlist enforced server-side.
5. **Fuzz the sidecar parsers** (`sbxproxy` HTTP/TLS decoders, `sbxfuse` path
   handling): they sit outside the workload but consume workload-controlled input,
   making them the prime privilege-escalation surface.
6. **Prefer firecracker** for workloads where a shared-kernel container escape is
   unacceptable.

---

_Scope note: this model covers the Hiver-authored components (sandbox sidecars,
controller, gateway). It assumes a correctly configured and trusted host, Docker
daemon, and Kubernetes control plane; defending those is the operator's
responsibility and out of scope here._
