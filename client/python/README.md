# Hiver Python Client

Python client for the Hiver runtime. Requires Python ≥ 3.11.

## Contents

- [📦 Installation](#-installation)
- [⚡ Quick start](#-quick-start)
- [📖 API](#-api)
  - [`get_or_create_sandbox`](#get_or_create_sandboxkey-config-kwargs)
  - [`list_sandboxes`](#list_sandboxeskwargs)
  - [`shutdown`](#shutdownsandbox-kwargs)
  - [`watch_sandbox_events`](#watch_sandbox_eventskwargs)
  - [`Sandbox`](#sandbox)
    - [`sandbox.ping()`](#sandboxping)
    - [`sandbox.get_ports()`](#sandboxget_ports)
    - [`sandbox.proxy_url()`](#sandboxproxy_urlport)
    - [`sandbox.get_config()`](#sandboxget_config)
    - [`sandbox.apply_config()`](#sandboxapply_configconfig)
    - [`sandbox.exec()`](#sandboxexeccommand-kwargs)
    - [`sandbox.exec_stream()`](#sandboxexec_streamcommand-kwargs)
    - [`sandbox.get_events_stream()`](#sandboxget_events_streamkwargs)
    - [`sandbox.list_directory()`](#sandboxlist_directorypath)
    - [`sandbox.upload_file()`](#sandboxupload_filedestination-filename-content)
    - [`sandbox.download_file()`](#sandboxdownload_filepath)
  - [Sandbox config](#sandbox-config)
  - [Egress rules & overrides](#egress-rules--overrides)
  - [Filesystems](#filesystems)
  - [Snapshots](#snapshots)
  - [Utils](#utils)
- [🧪 Examples](#-examples)

## 📦 Installation

```sh
pip install hiver-py
```

## ⚡ Quick start

```python
import asyncio
import hiver

async def main():
    sandbox = await hiver.get_or_create_sandbox(
        "my-sandbox",
        hiver.SandboxConfig(
            image="hiversh/python:3.13-alpine",
            ttl=1800,
            fs=[
                hiver.LocalFileSystem(
                    backend="local",
                    mount="/workspace",
                    acls=[hiver.ACLRule(path="/workspace/**", access="rw")],
                )
            ],
            egress=[
                hiver.EgressRule(
                    access="allow",
                    host="api.github.com",
                    methods=["GET"],
                    paths=["/repos/*"],
                )
            ],
        ),
    )

    print("sandbox id:", sandbox.id)

    # Run a command and read back the result.
    result = await sandbox.exec("echo hello from the sandbox")
    print(result["stdout"], result["exit_code"])

    # Stream events until done.
    abort = asyncio.Event()
    async for event in sandbox.get_events_stream(abort=abort):
        print("event", event)

    await hiver.shutdown(sandbox)

asyncio.run(main())
```

## 📖 API

### `get_or_create_sandbox(key, config?, **kwargs)`

Provisions a sandbox idempotently. If a sandbox with `key` already exists it is
returned unchanged and `config` is ignored; otherwise a new sandbox is created
from `config`. `config` is validated before the request is sent, so a bad config
fails fast on the caller side.

`key` must match `[A-Za-z0-9_-]{1,64}`. `config` is optional — when omitted the
sandbox defaults to a single read-write `/workspace` local mount and an allow-all
egress policy.

Returns a `Sandbox` handle once the sandbox is ready to accept requests.

```python
sandbox = await hiver.get_or_create_sandbox(
    "my-sandbox",
    config,
    gateway_url="http://localhost:10000",  # default
    timeout_s=60.0,                        # default; pass 0 to skip readiness wait
)
```

| Parameter     | Type    | Default                  | Description                                                              |
| ------------- | ------- | ------------------------ | ------------------------------------------------------------------------ |
| `gateway_url` | `str`   | `http://localhost:10000` | Base URL of the gateway. Also exported as `DEFAULT_GATEWAY_URL`.         |
| `timeout_s`   | `float` | `60.0`                   | Timeout for each request and the readiness poll. Pass `0` to skip waits. |

---

### `list_sandboxes(**kwargs)`

Lists all currently running sandboxes, returning a `Sandbox` handle for each.

```python
sandboxes = await hiver.list_sandboxes()
for sandbox in sandboxes:
    print(sandbox.key, sandbox.id)
```

---

### `shutdown(sandbox, **kwargs)`

Stops the sandbox container and removes it.

```python
await hiver.shutdown(sandbox)
```

---

### `watch_sandbox_events(**kwargs)`

Async generator over sandbox **lifecycle** events across the whole gateway
(`start`, `stop`, `die`, `destroy`). Distinct from
[`sandbox.get_events_stream()`](#sandboxget_events_streamkwargs), which streams
the runtime events of a single sandbox.

```python
abort = asyncio.Event()
async for event in hiver.watch_sandbox_events(abort=abort):
    print(event["status"], event["key"], event["id"])
```

Each event is a dict with `{ "id", "key", "status" }`.

---

### `Sandbox`

Returned by `get_or_create_sandbox` / `list_sandboxes`. Not constructed directly.

#### Properties

| Property         | Type  | Description                                                            |
| ---------------- | ----- | ---------------------------------------------------------------------- |
| `id`             | `str` | Server-assigned unique identifier (uuid).                              |
| `key`            | `str` | Caller-chosen key the sandbox was provisioned under; used for routing. |
| `api_server_url` | `str` | Base URL of the per-sandbox API server.                                |
| `mcp_endpoint`   | `str` | MCP endpoint URL for this sandbox.                                     |

Sandbox is also an async context manager — `await sandbox.aclose()` releases the
underlying HTTP client, or use `async with`:

```python
async with await hiver.get_or_create_sandbox("my-sandbox") as sandbox:
    ...
```

#### `sandbox.ping()`

Resets the sandbox TTL countdown.

```python
await sandbox.ping()
```

#### `sandbox.get_ports()`

Lists the TCP ports the sandbox exposes (the image's `EXPOSE` directives). Each
is reachable via [`proxy_url`](#sandboxproxy_urlport).

```python
ports = await sandbox.get_ports()  # e.g. [8080, 9000]
```

#### `sandbox.proxy_url(port)`

Returns the base proxy URL for a port inside the sandbox. Append a path to get a
full URL.

```python
import httpx
async with httpx.AsyncClient() as client:
    res = await client.get(sandbox.proxy_url(8080) + "/health")
```

#### `sandbox.get_config()`

Returns the current `SandboxConfig`.

```python
config = await sandbox.get_config()
```

#### `sandbox.apply_config(config)`

Applies a desired `SandboxConfig`. The server diffs it against the running state
and returns an `ApplyResult` whose `applied` field indicates whether the change
was committed or rolled back. Immutable fields (`image`, `cpu`, `memory`, `env`)
are preserved and reported in `changes.warnings`.

```python
current = await sandbox.get_config()
result = await sandbox.apply_config(
    current.model_copy(update={
        "egress": [
            hiver.EgressRule(access="allow", host="api.github.com", methods=["GET"], paths=["/repos/*"]),
            *(current.egress or []),
        ]
    })
)

if not result.applied:
    print("rolled back:", result.error)
else:
    print("changes:", result.changes)
```

#### `sandbox.exec(command, **kwargs)`

Runs `command` inside the sandbox and resolves once it finishes, returning
`{ "stdout", "stderr", "exit_code" }`.

```python
result = await sandbox.exec(
    "python3 -c 'print(6 * 7)'",
    cwd="/workspace",
    env={"PYTHONPATH": "/workspace"},
)
print(result["stdout"].strip(), result["exit_code"])
```

**Parameters**: `cwd`, `env`.

#### `sandbox.exec_stream(command, **kwargs)`

Runs `command` and returns an `ExecProcess` handle. Iterate `exec.pipes` for
incremental stdout/stderr, write to stdin via `exec.write_stdin()`, and await
the exit code via `exec.exit_code`. Pass `tty=True` for an interactive PTY
(stderr is merged into stdout).

```python
exec = await sandbox.exec_stream("python3", cwd="/workspace", tty=True)

await exec.write_stdin("print('the answer is', 6 * 7)\r")
await exec.write_stdin("exit()\r")

async for pipe in exec.pipes:
    if "stdout" in pipe:
        sys.stdout.write(pipe["stdout"])
    if "stderr" in pipe:
        sys.stderr.write(pipe["stderr"])

print("exit code:", await exec.exit_code)
```

**Parameters**: `cwd`, `env`, `tty`.

#### `sandbox.get_events_stream(**kwargs)`

Long-lived async generator over `SandboxEvent`s for this sandbox. Auto-resumes
across disconnects — if the SSE connection drops the generator silently
reconnects with the last observed id, so no events are missed.

```python
abort = asyncio.Event()

async for event in sandbox.get_events_stream(abort=abort):
    if event.type == "stdio":
        sys.stdout.write(event.stdout or "")
```

| Parameter       | Type            | Default | Description                                        |
| --------------- | --------------- | ------- | -------------------------------------------------- |
| `abort`         | `asyncio.Event` | —       | Set to stop the stream from the caller's side.     |
| `last_event_id` | `int`           | —       | Skip past this id on the first connect.            |
| `max_retries`   | `int`           | `3`     | Max reconnect attempts after a dropped connection. |

Event types include `stdio`, `exec.request` / `exec.response`,
`egress.request` / `egress.response` / `egress.chunk`,
`fs.request` / `fs.response`, `config.apply`, and `resource.usage`.

#### `sandbox.list_directory(path)`

Lists the immediate children of a directory under a sandbox mount. Returns a
list of entries with `name`, `path`, `is_dir`, and `size`.

```python
entries = await sandbox.list_directory("/workspace")
for e in entries:
    print("dir" if e["is_dir"] else "file", e["size"], e["path"])
```

#### `sandbox.upload_file(destination, filename, content)`

Uploads a file to a sandbox mount. `destination` must match a configured
`fs[].mount`. Returns `{ "path", "bytes" }`.

```python
result = await sandbox.upload_file("/workspace", "data.csv", csv_bytes)
print(f"uploaded {result['bytes']} bytes → {result['path']}")
```

`content` accepts `bytes` or `str`.

#### `sandbox.download_file(path)`

Downloads a file from a sandbox mount by its absolute path. Returns `bytes`.

```python
data = await sandbox.download_file("/workspace/output.json")
```

---

### Sandbox config

`SandboxConfig` describes the desired state of a sandbox. All fields are
optional.

| Field        | Type                       | Default            | Notes                                                            |
| ------------ | -------------------------- | ------------------ | ---------------------------------------------------------------- |
| `image`      | `str`                      | —                  | Agent image to launch. Immutable after init. Determines isolation (a microvm image ships a guest rootfs). |
| `cpu`        | `int`                      | `1`                | Virtual CPUs. Immutable after init.                              |
| `memory`     | `int`                      | `512`              | Memory in MiB. Immutable after init.                             |
| `entrypoint` | `str \| list[str]`         | image default      | Override the container entrypoint (argv list or whitespace-split string). |
| `env`        | `dict[str, str]`           | —                  | Extra environment variables. Immutable after init.               |
| `ttl`        | `int`                      | `1800`             | Idle TTL in seconds. Reset with `ping()`. `0` disables shutdown. |
| `fs`         | `list[FileSystem]`         | local `/workspace` | See [Filesystems](#filesystems).                                 |
| `egress`     | `list[EgressRule]`         | allow-all          | Ordered rules; see [Egress](#egress-rules--overrides).           |
| `snapshot`   | `Snapshot`                 | —                  | See [Snapshots](#snapshots).                                     |

---

### Egress rules & overrides

`egress` is an **ordered list** of rules. The first rule that matches a request
decides the outcome; requests that match no rule are denied. Each rule sets
`access: "allow" | "deny"` plus matchers (`host`, `ports`, `methods`, `paths`).

```python
egress=[
    hiver.EgressRule(
        access="allow",
        host="api.github.com",
        methods=["GET"],
        paths=["/repos/*"],
    ),
    hiver.EgressRule(access="deny", host="*"),  # catch-all
]
```

#### Overrides — inject secrets the agent never sees

The `override` field on an `allow` rule injects values into every matching
outbound request before it leaves the sandbox. This keeps API keys out of
agent-visible environment variables or command output. If the agent already set
the same header/query parameter, the proxy overwrites it.

```python
sandbox = await hiver.get_or_create_sandbox(
    "my-sandbox",
    hiver.SandboxConfig(
        egress=[
            hiver.EgressRule(
                access="allow",
                host="api.example.com",
                paths=["/v1/*"],
                override=hiver.EgressOverride(
                    headers={"Authorization": "Bearer sk-live-abc123"},
                    query={"api_key": "sk-live-abc123"},
                ),
            )
        ]
    ),
)
```

The agent can call `curl https://api.example.com/v1/data` with no credentials —
Hiver appends the `Authorization` header and `?api_key=…` transparently.

---

### Filesystems

The `fs` array configures one or more mounts visible inside the sandbox. Mount
paths must be unique and non-overlapping. Access is governed by `acls`, evaluated
longest-prefix-first with deny as the default.

#### Local

```python
fs=[
    hiver.LocalFileSystem(
        backend="local",
        mount="/workspace",
        acls=[hiver.ACLRule(path="/workspace/**", access="rw")],
        origin="./local-dir",  # optional; Docker runtime only
    )
]
```

Files live only for the lifetime of the sandbox (unless captured via a
[snapshot](#snapshots)). The optional `origin` mounts a host directory into the
sandbox — handy in local development. Local origins are only supported with the
Docker runtime.

#### Google Drive

Mount a Google Drive folder so every file the agent writes persists to Drive and
survives sandbox restarts.

```python
fs=[
    hiver.GDriveFileSystem(
        backend="gdrive",
        mount="/workspace",
        acls=[hiver.ACLRule(path="/workspace/**", access="rw")],
        gdrive_access_token="<oauth-access-token>",
        gdrive_refresh_token="<oauth-refresh-token>",
        gdrive_client_id="<google-client-id>",
        gdrive_client_secret="<google-client-secret>",
        gdrive_folder_id="<drive-folder-id>",
    )
]
```

**OAuth tokens** — obtain via the [Google OAuth 2.0 flow](https://developers.google.com/identity/protocols/oauth2)
with the `https://www.googleapis.com/auth/drive` scope. `gdrive_refresh_token` is
used to renew the access token automatically. Alternatively, supply
`gdrive_service_account_json` instead of the OAuth fields.

**`gdrive_folder_id`** — the ID of the Drive folder to expose as the mount root.
Find it in the folder's URL: `https://drive.google.com/drive/folders/<folder-id>`.
When omitted, the account root is used.

#### Google Cloud Storage

Mount a GCS bucket (or a prefix within one) so files persist beyond sandbox
lifetime.

```python
fs=[
    hiver.GCSFileSystem(
        backend="gcs",
        mount="/workspace",
        acls=[hiver.ACLRule(path="/workspace/**", access="rw")],
        gcs_bucket="<bucket-name>",
        gcs_prefix="optional/key/prefix",  # optional
        gcs_service_account_json=json.dumps(service_account_key),
    )
]
```

---

### Snapshots

A snapshot captures part of the sandbox's filesystem automatically before
shutdown and restores it before the next start — even for paths outside a
host-backed mount.

```python
config = hiver.SandboxConfig(
    image="hiversh/python:3.13-alpine",
    snapshot=hiver.Snapshot(
        restore_key="session-42",   # restored on start
        write_key="session-42",     # saved on shutdown; defaults to restore_key
        include=["/root/**"],       # glob paths to capture
    ),
)
```

Boot the sandbox under the same `restore_key` later to bring the captured files
back.

---

### Utils

#### `allowed_python_packages`

Generates the egress rules needed to let the sandbox install specific Python
packages via pip.

```python
sandbox = await hiver.get_or_create_sandbox(
    "my-sandbox",
    hiver.SandboxConfig(
        egress=hiver.allowed_python_packages("numpy", "pandas", "matplotlib"),
    ),
)
```

Only the packages you name are allowed through — any `pip install` for an
unlisted package is blocked.

#### `allowed_npm_packages`

Generates the egress rules needed to let the sandbox install specific npm
packages.

```python
sandbox = await hiver.get_or_create_sandbox(
    "my-sandbox",
    hiver.SandboxConfig(
        egress=hiver.allowed_npm_packages("lodash"),
    ),
)
```

---

## 🧪 Examples

Run any example with `python examples/<name>.py` from `client/python/`.

| Example                 | What it shows                                                   |
| ----------------------- | --------------------------------------------------------------- |
| `python_exec_stream.py` | Run a Python function in the sandbox and stream output via SSE. |
| `python_exec_tty.py`    | Drive an interactive Python REPL over a TTY exec stream.        |
