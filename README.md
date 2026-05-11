# Hive Agent Sandbox

Hive gives an agent a self-contained pod — its own filesystem, its own
network exit — with every read, write, and outbound request mediated
and audited by a small set of sidecars. The agent itself is unmodified
and unaware: a plain container image, drop-in replaceable, doesn't have
to opt into anything.

## How an agent sees the world

```
agent: GET https://api.github.com/user        ─▶ rule check ─▶ proxy fetches ─▶ response
agent: POST https://example.com (no rule)     ─▶ 403, audited
agent: open("/workspace/hello.txt")           ─▶ ACL check ─▶ allow + audit
agent: open("/workspace/secret/keys.txt")     ─▶ ACL deny ─▶ ENOENT + audit
host:  curl -d 'ls /workspace' :18000/exec    ─▶ agent runs cmd, returns json
```

The agent makes plain TCP and plain `open()` calls. Nothing in the
agent image knows the proxy or the FUSE mount exist.

## Quick start

```bash
# Bring up a pod against one of the bundled fixtures (won't auto-stop).
./test/e2e/run-fixture.sh agent-python   # or agent-node

# Once it prints "sandbox-pod is running":
docker logs -f sandbox-pod-agent-python
docker exec  -it sandbox-pod-agent-python bash
curl -d 'echo hi; ls /workspace' http://localhost:18000/exec | jq
```

A full E2E run with assertions:

```bash
go test -v ./test/e2e/...
```

## What gets enforced

Every fixture lives in `test/e2e/fixtures/<name>/` and ships its own
`spec.yaml` declaring what the agent is allowed to do.

```yaml
egress:
  allow:
    - host: api.github.com
      methods: [GET]
      paths: [/repos/*, /user]
      headers:
        X-Sandbox: hive

    - host: go.dev
      methods: [GET]
      paths: [/solutions/case-studies/*] # TLS is intercepted to enforce paths

fs:
  backend: local
  mount: /workspace
  acls:
    - { path: /workspace, access: rw }
    - { path: /workspace/secret/**, access: deny }
```

- **Host** — exact (`api.github.com`) or wildcard suffix (`*.pypi.org`).
- **Methods / Paths** — optional filters; `[GET]`/`[/api/*]`. Empty
  list means "any". Path-level rules force TLS interception via a
  per-pod CA that sandboxd splices into the agent's trust store.
- **Headers** — merged into the forwarded request via `Header.Set`;
  the agent can't see or override them.
- **ACLs** — longest-prefix match, default-deny, `deny` reads return
  `ENOENT` (the path simply doesn't appear).

See [`test/e2e/fixtures/agent-python/spec.yaml`](test/e2e/fixtures/agent-python/spec.yaml)
for the working reference.

## What's inside the pod

| Process    | Job                                                                                                                                                                                                                             |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------- |
| `sandboxd` | Generates a per-pod CA, stands up the sidecars, installs iptables OUTPUT REDIRECT, unpacks the agent rootfs, splices the CA into its trust store, runs `runc`, and tails proxy + FUSE audit logs as `sandboxd: agent op         | …` lines on stdout. |
| `sbxproxy` | Transparent TCP intercept (via SO_ORIGINAL_DST). Sniffs HTTP vs TLS. HTTP gets full method/path/header matching; TLS gets host-only matching from SNI, with optional MITM termination when path/method/header rules require it. |
| `sbxfuse`  | bazil/fuse passthrough mount over a host backend, longest-prefix-match ACLs.                                                                                                                                                    |

Every mediated operation produces a JSON-line audit event in
`/audit-out/{proxy,fuse}.log`; sandboxd also surfaces them on stdout
in human-readable form so `docker logs` is a useful debugging view.

## Layout

```
go.mod
Dockerfile                                # sandbox-runtime: sandboxd + sidecars + runc + iptables
cmd/
  sandboxd/                               # spec-driven orchestrator
  sbxproxy/                               # MITM proxy
  sbxfuse/                                # FUSE workspace
internal/
  proxy/                                  # HTTP/TLS handling, allowlist match, CA + cert minter
  fusefs/                                 # FUSE Server, ACL trie
  runc/                                   # docker-archive parser + OCI bundle generator
  spec/                                   # YAML / JSON spec types
test/
  e2e/
    common.go                             # runFixtureE2E orchestration
    sandbox_test.go                       # one test per fixture
    run-fixture.sh                        # bring up a pod for inspection
    fixtures/
      agent-python/  agent-node/          # the bundled fixtures
```

## Status

Prototype. The architecture rationale and the open design questions
live in [DESIGN.md](docs/DESIGN.md)

## Testing

```bash
go test ./internal/...           # unit tests, run anywhere (no docker needed)
go test ./test/e2e/...           # full E2E, requires a reachable docker daemon
```
