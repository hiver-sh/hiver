# Agent Sandbox — Platform Requirements

Each requirement carries a stable identifier (`REQ-N`). Identifiers are referenced from `DESIGN.md`, tests, and traceability tooling — do not renumber existing entries; append new ones at the end of their section.

## Control-Plane API
- **R1**: Expose a versioned REST + gRPC API (with an OpenAPI/Protobuf spec) as the single entry point for creating, configuring, and tearing down ephemeral sandboxes
- **R2**: `POST /v1/sandboxes` accepts a declarative spec covering: base image, resource limits (CPU, memory, disk, timeout), isolation backend (container, microVM, gVisor, Kata)
- **R3**: Spec includes a `security` block: seccomp profile, capability set, user/UID, read-only root, kernel feature toggles
- **R4**: Spec includes a `filesystem` block: workspace mounts, FUSE backend (local, S3, GCS, encrypted volume), per-path ACLs, quotas, COW overlay flags
- **R5**: Spec includes an `egress` block: domain allowlist with method/path rules, TLS interception toggle, rate limits, request/response inspection rules, brokered credential bindings
- **R6**: Idempotent creation via client-supplied request IDs; return a sandbox handle with attach/exec/upload/download endpoints
- **R7**: `PATCH /v1/sandboxes/{id}` for live policy updates (tightening only — loosening requires a new sandbox to preserve audit integrity)
- **R8**: `DELETE /v1/sandboxes/{id}` triggers guaranteed teardown: kill processes, unmount FUSE, flush audit logs, destroy ephemeral storage
- **R9**: `GET /v1/sandboxes/{id}/logs` retrieves session logs with filters (`type=stdout|stderr|network|filesystem|audit`, `since`, `until`, `limit`, `cursor`) and supports both paginated JSON and newline-delimited streaming (`?follow=true` for tail-style live output)
- **R10**: `POST /v1/sandboxes/{id}/exec` runs a one-shot command (`{argv, env, cwd, stdin, timeout}`) and returns `{exitCode, stdout, stderr, durationMs}`; honors all sandbox policies (egress, FUSE ACLs, resource limits) and emits an audit event
- **R11**: Authentication via OIDC / mTLS / signed JWTs; authorization via per-tenant RBAC with policies stored as code and reviewable
- **R12**: Per-call audit log entry capturing actor, spec hash, decision, and resulting sandbox ID
- **R13**: Rate limits and quotas at the API layer (per tenant, per user, per API key) to prevent abuse
- **R14**: Client SDKs in Python, TypeScript, and Go, generated from the spec, with examples for the common patterns (one-shot task, long-running session, batch fan-out)
- **R15**: Reusable named **sandbox profiles** stored server-side so callers can `POST /v1/sandboxes {profile: "code-review-strict"}` without re-sending the full spec
- **R16**: Dry-run / validate mode (`?dryRun=true`) returns the resolved policy without creating a sandbox, for CI gating and review

## Isolation & Execution
- **R17**: Run each agent session in an isolated environment (container, microVM, gVisor, or Kata) with no access to host filesystem or other sessions
- **R18**: Enforce per-session resource limits: CPU, memory, disk, process count, and wall-clock time
- **R19**: Use an ephemeral, copy-on-write root filesystem that is destroyed at session end (the agent workspace is mounted separately via FUSE — see below)
- **R20**: Drop privileges by default (non-root user, no capabilities, read-only system directories)
- **R21**: Apply seccomp/AppArmor/SELinux profiles to restrict syscalls
- **R22**: Prevent container escape via user namespaces and rootless runtimes where possible

## Network Egress via MITM Proxy
- **R23**: Force **all** outbound HTTP(S) traffic through a transparent MITM proxy — no direct egress permitted
- **R24**: Block raw TCP/UDP/ICMP egress at the network namespace level (only the proxy port is reachable)
- **R25**: Inject a per-session CA certificate into the sandbox trust store so TLS interception works without client warnings
- **R26**: Generate a unique CA per session (or per tenant) — never share CAs across tenants
- **R27**: Strip the CA private key from the sandbox; only the proxy holds it
- **R28**: Pin the proxy as the only resolvable DNS path, or run DNS through the proxy to prevent DNS exfiltration
- **R29**: Block IPv6 unless explicitly proxied to avoid bypass via dual-stack
- **R30**: Block access to cloud instance metadata endpoints (`169.254.169.254`, `fd00:ec2::254`) at the network layer regardless of proxy policy

## Proxy Policy & Inspection
- **R31**: Allowlist destinations by domain (and optionally path/method) — deny by default
- **R32**: Per-agent or per-task policies (e.g. a research agent can hit `*.wikipedia.org`; a code agent can hit `github.com`, `pypi.org`)
- **R33**: Rate-limit per destination, per session, and per tenant
- **R34**: Block uploads above configurable size to prevent data exfiltration
- **R35**: **Never pass credentials as environment variables to the agent.** Secrets are registered at sandbox creation as references (vault paths, KMS keys) bound to specific destinations; the proxy resolves them and injects the auth header on outbound requests at the matching hop — the agent process never sees the raw value
- **R36**: Configurable handling of inbound auth-like headers from the agent (`Authorization`, `Cookie`, `X-Api-Key`, etc.): per-policy choice of `strip`, `reject`, or `passthrough`; default is `strip` so only proxy-injected credentials are forwarded, with `passthrough` reserved for trusted destinations that explicitly require agent-supplied auth
- **R37**: Scrub credential values from request/response logs (replace with a stable hash) so audit records remain useful without leaking secrets
- **R38**: Support content-type allowlists (e.g. block executables from being downloaded)

## Logging, Audit & Replay
- **R39**: Log every event — network request/response, filesystem operation, process exec, API call, policy decision — to a unified audit pipeline with consistent schema (timestamp, session ID, tenant ID, actor, event type, details)
- **R40**: Persist full request/response bodies and file contents (subject to retention policy) for forensic review
- **R41**: Tamper-evident logs (append-only, hash-chained, or shipped to a write-once store)

## Secrets & Credentials
- **R42**: Never expose raw API keys to the agent — the API spec accepts only credential *references* (vault paths, KMS keys), and the proxy injects the actual auth header on outbound calls
- **R43**: Scope credentials per session; rotate frequently; revoke on session end and on any policy violation
- **R44**: Scan agent-generated content for secrets before egress and before persistence; quarantine flagged content via the same path as FUSE inline scanning

## Filesystem & Data Boundaries
- **R45**: Mount only the explicit working directory the user authorized
- **R46**: Block access to `~/.ssh`, `~/.aws`, and other sensitive host paths via FUSE ACLs (see FUSE Filesystem Support)
- **R47**: Provide a clean, signed artifact channel for files the agent produces

## FUSE Filesystem Support
- **R48**: Expose the agent workspace through a userspace FUSE mount so every read/write/stat/unlink call is mediated by the platform, not the kernel directly
- **R49**: Enforce per-path ACLs in the FUSE layer: read-only system paths, read-write workspace, deny-listed sensitive paths returned as `ENOENT`
- **R50**: Log every filesystem operation (path, op, size, pid, timestamp) to the same audit pipeline as network events for unified replay
- **R51**: Support copy-on-write overlays so the agent sees a writable view while the underlying source remains immutable; discard the overlay at session end
- **R52**: Run the FUSE daemon out-of-process and unprivileged; treat a daemon crash as fail-closed (workspace becomes inaccessible, session paused)
- **R53**: Support pluggable backends (local disk, S3/GCS, encrypted volume) behind the same FUSE interface so policy is backend-agnostic
- **R54**: Snapshot the workspace on demand for forensic capture and reproducible session replay
- **R55**: Cross-platform FUSE support: macFUSE on macOS, libfuse3 on Linux; abstract the differences behind a single mount API so policy and audit behavior are identical on both
- **R56**: On Kubernetes, run the FUSE daemon as a sidecar (or DaemonSet with CSI driver) using `bidirectional` mount propagation so the agent container sees the mediated mount without granting it `SYS_ADMIN`

## Deployment & Portability
- **R57**: Run identically on a developer laptop (macOS, Linux) and in production (on-prem or cloud Kubernetes) — same images, same policies, same audit format
- **R58**: Provide a single-binary or `docker compose` local mode for fast iteration; feature parity with the production deployment except for scale
- **R59**: Ship a Helm chart (and/or Kustomize overlays) covering: sandbox runtime, MITM proxy, FUSE CSI driver, audit pipeline, policy service
- **R60**: Support common Kubernetes distributions (EKS, GKE, AKS, OpenShift, vanilla upstream) and air-gapped on-prem clusters with a private registry
- **R61**: Abstract container runtime: works with containerd, CRI-O, and Docker; use Kata or gVisor where stronger isolation is required
- **R62**: Provide Terraform modules for cloud deployments (VPC, subnets, KMS keys, object storage buckets, IAM policies)
- **R63**: Externalize all state (audit logs, snapshots, policies) to pluggable backends — no reliance on a specific cloud provider's proprietary services
- **R64**: CI matrix that exercises macOS, Linux (amd64 + arm64), and Kubernetes end-to-end on every release

## Observability & Control
- **R65**: Real-time stream of agent actions (tool calls, file edits, network requests) to the user
- **R66**: User-initiated kill switch with guaranteed termination of all session processes
- **R67**: Per-action approval mode for high-risk operations (writes outside workspace, new network destinations)
- **R68**: Anomaly detection across all signals: spikes in egress volume, new domains, unusual filesystem activity (mass reads/writes, access to rarely-touched paths), and abnormal API usage patterns

## Multi-Tenancy & Compliance
- **R69**: Hard tenant isolation at network, storage, and compute layers
- **R70**: Configurable data residency and log retention to meet GDPR/SOC2/HIPAA requirements
- **R71**: Per-tenant policy overrides without weakening global defaults
- **R72**: Reproducible, versioned sandbox images with provenance/SBOM
