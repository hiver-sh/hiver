# Sandbox Pod — Prototype

A runnable prototype of the sandbox pod from [DESIGN.md](./DESIGN.md). Two **independent** images, joined at runtime by `runc`:

- **`sandbox-runtime` image** (root [`Dockerfile`](./Dockerfile)) — `sandboxd` + `sbxproxy` + `sbxfuse` + `runc`. No language runtime. Same image whether the agent is Python, Node, Go, or Rust.
- **Agent image** (e.g. [`test/e2e/fixtures/agent-python/`](./test/e2e/fixtures/agent-python)) — a normal application image (`FROM python:3.12-slim`) whose `ENTRYPOINT` is the agent script. **It does not extend `sandbox-runtime`.** It has no idea sandboxd exists.
- **Host-side Go test** ([`test/e2e/sandbox_test.go`](./test/e2e/sandbox_test.go)) — builds both images, `docker save`s the agent into a tarball, and bind-mounts that tarball into the sandbox-pod container. sandboxd unpacks the tar into an OCI rootfs and launches the agent under `runc` — the agent runs in **its own container**, not as a child process of sandboxd.

## How the pieces fit together

```
[Host]                                       [sandbox-pod container — sandbox-runtime]
                                             ┌─────────────────────────────────────┐
go test ./test/e2e/...                       │ sandboxd --spec /mnt/spec.json      │
  │                                          │   ├── sbxproxy (allowlist)          │
  ├── docker build sandbox-runtime           │   ├── sbxfuse  (FUSE on /workspace) │
  ├── docker build agent-python              │   ├── unpack /mnt/agent.tar →       │
  ├── docker save agent-python → agent.tar   │   │     /tmp/bundle/rootfs/         │
  │                                          │   ├── write /tmp/bundle/config.json │
  ├── start httptest upstreams (host)        │   │     (shared netns, /workspace   │
  │     0.0.0.0:N₁  (allowed)                │   │      bind-mount)                │
  │     0.0.0.0:N₂  (denied)                 │   └── runc run -b /tmp/bundle agent │
  │                                          │           │ (separate container)    │
  ├── render spec.json                       │           ▼                         │
  │     env=[ALLOW_URL, DENY_URL, …]         │         ┌──────────────────────┐    │
  │                                          │         │ agent container      │    │
  ├── docker run --privileged                │         │   FROM python:3.12   │    │
  │     --add-host upstream-{allowed,denied} │         │   ENTRYPOINT=        │    │
  │     -v auditDir:/audit-out               │         │     python3 /agent.py│    │
  │     -v agent.tar:/mnt/agent.tar          │         │   netns: shared ↑    │    │
  │     -v spec.json:/mnt/spec.json          │         │   /workspace: bind ↑ │    │
  │     sandbox-runtime --spec /mnt/spec.json│         └──────────────────────┘    │
  ├── parse stdout                           │                                     │
  ├── read auditDir/{proxy,fuse}.log         │   audit logs → /audit-out (bind)    │
  └── assert                                 └─────────────────────────────────────┘
```

The two HTTP upstreams **run on the host**, on different ports. Inside the sandbox-pod container they're aliased via Docker's `host-gateway`:

```
upstream-allowed  → host (matches spec.egress.allow)
upstream-denied   → host (NOT in allowlist; proxy 403s)
```

Both names resolve to the host, but the proxy filters by Host-header, so allowed/denied are distinguishable. The agent container inherits the sandbox-pod's netns, so its loopback (where `sbxproxy` listens) and `/etc/hosts` (where the upstream aliases live, used by sbxproxy) are both shared.

## sandboxd is spec-driven

`sandboxd` takes one flag, `--spec <path>`, and reads a JSON spec (see [`internal/spec/spec.go`](./internal/spec/spec.go)). The agent is identified by an **image tarball**, not a binary:

```json
{
  "agent": {
    "image_tar": "/mnt/agent.tar",
    "env": ["ALLOW_URL=...", "DENY_URL=...", "DENY_PATH=..."]
  },
  "workspace": {
    "backend": "/workspace-backend",
    "mount":   "/workspace",
    "acls": [
      {"path": "/",          "access": "rw"},
      {"path": "/**",        "access": "rw"},
      {"path": "/secret/**", "access": "deny"}
    ]
  },
  "egress":    {"allow": ["upstream-allowed"]},
  "audit_dir": "/audit-out"
}
```

`agent.image_tar` is a `docker save` archive bind-mounted into the sandbox-pod. The agent's command, base env, and working dir come from the image config; `agent.env` is appended.

Adding a Node agent means a sibling fixture: `test/e2e/fixtures/agent-node/` with a `Dockerfile` (`FROM node:20-slim` + `COPY agent.js` + `ENTRYPOINT ["node", "/agent.js"]`). The `sandbox-runtime` image stays untouched, and so does sandboxd.

## How to run the test

```bash
# Anywhere with a Docker daemon reachable (macOS, Linux):
go test -count=1 -v -run TestSandboxPodE2E ./test/e2e/...
```

The test itself does the `docker build` for both images. Cross-platform unit tests (proxy, ACL evaluator, spec loader) still run natively without Docker:

```bash
go test ./internal/...
```

## What the E2E test asserts

Four signal classes from one `docker run`:

| Signal source                  | Where it comes from                          | What's checked                                                       |
|--------------------------------|----------------------------------------------|----------------------------------------------------------------------|
| **Agent script output**        | `[agent:out]` lines in container stdout      | `http_allow_get → 200`, `http_deny_get → 403`, `fs_write_rw OK`, `fs_read_rw OK len=17`, `fs_read_denied → ENOENT` |
| **Proxy audit log**            | `<auditDir>/proxy.log` via host bind-mount   | At least one `allow` and one `deny` verdict                          |
| **FUSE audit log**             | `<auditDir>/fuse.log` via host bind-mount    | A `write/allow` somewhere; a `deny` on `/secret/...`                 |
| **sandboxd lifecycle logs**    | container stdout (sandboxd's own `log.Printf`) | `audit dir = …`, `[sbxproxy:err] sbxproxy listening on …`, `[sbxfuse:err] sbxfuse: mounted …`, `agent image unpacked to …`, `[agent:out] …`, `agent finished, shutting down sidecars` |

## Layout

```
go.mod
Dockerfile                                # sandbox-runtime: sandboxd + sbxproxy + sbxfuse + runc
cmd/
  sandboxd/main.go                        # spec-driven (--spec only); launches agent under runc
  sbxproxy/main.go
  sbxfuse/main.go
internal/
  proxy/                                  # HTTP forward + CONNECT, allowlist, audit
  fusefs/                                 # bazil/fuse passthrough + ACL trie (Linux); ACL evaluator cross-platform
  runc/                                   # docker-archive parser + OCI bundle generator
  spec/                                   # Spec types + JSON loader
test/
  e2e/
    sandbox_test.go                       # host-side, docker-orchestrated
    fixtures/
      agent-python/
        Dockerfile                        # FROM python:3.12-slim, ENTRYPOINT=python3 /agent.py
        agent.py                          # the workload
```

## Adding another language fixture

Step-by-step for, say, Node.js:

```bash
mkdir -p test/e2e/fixtures/agent-node
cat > test/e2e/fixtures/agent-node/Dockerfile <<'EOF'
FROM node:20-slim
COPY agent.js /agent.js
ENTRYPOINT ["node", "/agent.js"]
EOF
# write agent.js
```

The test would change one constant (`agentImage`) and the agent-output assertions; everything else stays. There is **no** sandboxd-specific scaffolding inside the agent image.

## Why runc

The prototype uses `runc` (not `docker run` or `nsenter`) to launch the agent for the same reason production will: sandboxd needs to own the agent's namespaces, capabilities, and bind mounts directly. The current bundle generator (`internal/runc`) writes a runtime spec that:

- creates fresh `pid`/`mount`/`ipc`/`uts` namespaces but **inherits the sandbox-pod's netns**, so `sbxproxy` on `127.0.0.1:<port>` is reachable and Host-header allowlisting still works;
- bind-mounts the FUSE-backed `/workspace` from the sandbox-pod into the agent, so `sbxfuse` continues to mediate every read/write;
- drops to a small capability set with `noNewPrivileges` (rootless is a follow-up).

Running `runc` inside Docker requires `--privileged` for now (cgroup setup + namespace creation). Trimming that to the minimum cap set is tracked separately.

## Mapping to DESIGN.md tickets

| Ticket | Status in prototype | Notes |
|--------|---------------------|-------|
| T1 — repo scaffolding | ✓ | go.mod, cmd/, internal/, deploy assets via Dockerfile |
| T11 — spec types | ✓ partial | `internal/spec` covers the prototype-needed subset of §5 |
| T47 — sandboxd skeleton | ✓ prototype | spawns sidecars + launches agent under runc in its own container |
| T49 — runc isolation backend | ✓ partial | runc bundle generated from docker-archive; new pid/mount/ipc/uts ns; netns shared with sandbox-pod |
| T56 — sbxproxy binary | ✓ | HTTP + CONNECT, allowlist, audit |
| T58 — egress allowlist + cache | ✓ partial | exact + `*.suffix`; no LRU yet |
| T59 — inbound auth header strip | ✓ | strip only |
| T68 — proxy audit emission | ✓ | one event per decision |
| T69 — sbxfuse Linux mounter | ✓ | bazil/fuse passthrough |
| T75 — ACL trie | ✓ | longest-prefix-match; deny → ENOENT (incl. readdir hide) |
| T79 — FUSE audit emission | ✓ | one event per op |
| T117 — integration tests | ✓ | per-package tests in `internal/...` |
| T118 — E2E with kind | ✗ | uses raw `docker run` — kind comes later |

The `implement-ticket` skill (in `.claude/skills/`) drives further work — invoking `/implement-ticket T48` would open the proper Runtime interface ticket against this prototype, etc.
