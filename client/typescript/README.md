# Hiver TypeScript Client

TypeScript client for Hiver runtime. Works in Node.js (≥18) and Bun.

## Contents

- [📦 Installation](#-installation)
- [⚡ Quick start](#-quick-start)
- [📖 API](#-api)
  - [`getOrCreateSandbox`](#getorcreatesandboxkey-config-opts)
  - [`listSandboxes`](#listsandboxesopts)
  - [`shutdown`](#shutdownsandbox-opts)
  - [`watchSandboxEvents`](#watchsandboxeventsopts-signal)
  - [`Sandbox`](#sandbox)
    - [`sandbox.ping()`](#sandboxping)
    - [`sandbox.getPorts()`](#sandboxgetports)
    - [`sandbox.proxyUrl()`](#sandboxproxyurlport)
    - [`sandbox.getConfig()`](#sandboxgetconfig)
    - [`sandbox.applyConfig()`](#sandboxapplyconfigconfig)
    - [`sandbox.exec()`](#sandboxexeccommand-opts)
    - [`sandbox.execStream()`](#sandboxexecstreamcommand-opts)
    - [`sandbox.getEventsStream()`](#sandboxgeteventsstreamopts)
    - [`sandbox.listDirectory()`](#sandboxlistdirectorypath)
    - [`sandbox.uploadFile()`](#sandboxuploadfilepath-content)
    - [`sandbox.downloadFile()`](#sandboxdownloadfilepath)
  - [Sandbox config](#sandbox-config)
  - [Egress rules & overrides](#egress-rules--overrides)
  - [Filesystems](#filesystems)
  - [Snapshots](#snapshots)
  - [Utils](#utils)
- [🖥️ Exec](#️-exec)
- [🧪 Examples](#-examples)

## 📦 Installation

```sh
npm install --save @hiver.sh/client
```

## ⚡ Quick start

```ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("my-sandbox", {
  image: "node",
  ttl: 1800,
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  egress: [
    {
      access: "allow",
      host: "api.github.com",
      methods: ["GET"],
      paths: ["/repos/*"],
    },
  ],
});

console.log("sandbox id:", sandbox.id);

// Run a command and read back the result.
const result = await sandbox.exec("echo hello from the sandbox");
console.log(result.stdout, result.exit_code);

// Keep the sandbox alive.
const ping = setInterval(sandbox.ping, 10_000);

// Stream events until done.
const ac = new AbortController();
for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  console.log("event", event);
}

clearInterval(ping);
await hiver.shutdown(sandbox);
```

## 📖 API

### `getOrCreateSandbox(key, config?, opts?)`

Provisions a sandbox idempotently. If a sandbox with `key` already exists it is
returned unchanged and `config` is ignored; otherwise a new sandbox is created
from `config`. `config` is validated against the `SandboxConfig` schema before
the request is sent, so a bad config fails fast on the caller side.

`key` must match `[A-Za-z0-9_-]{1,64}`. `config` is optional — when omitted (or
when `fs`/`egress` are omitted) the sandbox defaults to a single read-write
`/workspace` local mount and an allow-all egress policy.

Returns a `Sandbox` handle once the sandbox is ready to accept requests.

```ts
const sandbox = await hiver.getOrCreateSandbox("my-sandbox", config, {
  gatewayUrl: "http://localhost:10000", // default
  timeoutMs: 60_000, // default; pass 0 to skip the readiness wait
});
```

**`GatewayOptions`**

| Field        | Type           | Default                  | Description                                                                                             |
| ------------ | -------------- | ------------------------ | ------------------------------------------------------------------------------------------------------- |
| `gatewayUrl` | `string`       | `http://localhost:10000` | Base URL of the gateway. Also exported as `DEFAULT_GATEWAY_URL`.                                        |
| `fetch`      | `typeof fetch` | global `fetch`           | Override for testing or custom transports.                                                              |
| `timeoutMs`  | `number`       | `60000`                  | Timeout applied to each controller fetch and to the readiness poll. Pass `0` to disable and skip waits. |

---

### `listSandboxes(opts?)`

Lists all currently running sandboxes, returning a `Sandbox` handle for each.

```ts
const sandboxes = await hiver.listSandboxes();
for (const sandbox of sandboxes) {
  console.log(sandbox.key, sandbox.id);
}
```

---

### `shutdown(sandbox, opts?)`

Stops the sandbox container and removes it.

```ts
await hiver.shutdown(sandbox);
```

---

### `watchSandboxEvents(opts?, signal?)`

Async iterator over sandbox **lifecycle** events across the whole gateway
(`start`, `stop`, `die`, `destroy`). Distinct from
[`sandbox.getEventsStream()`](#sandboxgeteventsstreamopts), which streams the
runtime events of a single sandbox.

```ts
const ac = new AbortController();
for await (const event of hiver.watchSandboxEvents({}, ac.signal)) {
  console.log(event.status, event.key, event.id);
}
```

Each `SandboxLifecycleEvent` carries `{ id, key, status }`.

---

### `Sandbox`

Returned by `getOrCreateSandbox` / `listSandboxes`. Not constructed directly.

#### Properties

| Property       | Type                                 | Description                                                            |
| -------------- | ------------------------------------ | ---------------------------------------------------------------------- |
| `id`           | `string`                             | Server-assigned unique identifier (uuid).                              |
| `key`          | `string`                             | Caller-chosen key the sandbox was provisioned under; used for routing. |
| `apiServerUrl` | `string`                             | Base URL of the per-sandbox API server.                                |
| `proxyUrl`     | `(port: number \| string) => string` | Builds the base proxy URL for a port exposed inside the sandbox.       |
| `fetchImpl`    | `typeof fetch`                       | The `fetch` implementation this handle uses.                           |

#### `sandbox.ping()`

Resets the sandbox TTL countdown. Bound as an arrow function so
`setInterval(sandbox.ping, 10_000)` works without `.bind`.

```ts
await sandbox.ping();
// or
const interval = setInterval(sandbox.ping, 10_000);
```

#### `sandbox.getPorts()`

Lists the TCP ports the sandbox exposes (the image's `EXPOSE` directives). Each
is reachable via [`proxyUrl`](#sandboxproxyurlport).

```ts
const ports = await sandbox.getPorts(); // e.g. [8080, 9000]
```

#### `sandbox.proxyUrl(port)`

Returns the base proxy URL for a port inside the sandbox. It ends with a
trailing slash, so it reaches the port's root as-is; append a path to reach an
endpoint.

```ts
const res = await fetch(`${sandbox.proxyUrl(8080)}health`);
```

#### `sandbox.getConfig()`

Returns the current `SandboxConfig`.

```ts
const config = await sandbox.getConfig();
```

#### `sandbox.applyConfig(config)`

Applies a desired `SandboxConfig`. The server diffs it against the running
state; returns an `ApplyResult` whose `applied` field indicates whether the
change was committed or rolled back. Some fields (e.g. `image`, `cpu`, `memory`,
`env`) cannot be changed after the sandbox is initialized — these are preserved
and reported in `changes.warnings`.

```ts
const current = await sandbox.getConfig();
const result = await sandbox.applyConfig({
  ...current,
  egress: [
    {
      access: "allow",
      host: "api.github.com",
      methods: ["GET"],
      paths: ["/repos/*"],
    },
    ...(current.egress ?? []),
  ],
});

if (!result.applied) {
  console.error("rolled back:", result.error);
} else {
  console.log("changes:", result.changes);
}
```

#### `sandbox.exec(command, opts?)`

Runs `command` inside the sandbox and resolves once it finishes, returning
buffered `{ stdout, stderr, exit_code }`.

```ts
const result = await sandbox.exec('node -e "console.log(6 * 7)"', {
  cwd: "/workspace",
  env: { NODE_ENV: "production" }, // merged on top of the config's env
});
console.log(result.stdout.trim(), result.exit_code);
```

**`ExecOptions`**: `cwd`, `env`, `signal`, `timeoutMs`.

#### `sandbox.execStream(command, opts?)`

Runs `command` and returns an `ExecProcess` handle for streaming. Iterate
`exec.pipes` for incremental stdout/stderr, write to stdin via
`exec.writeStdin()`, and await the exit code via `exec.exitCode`. The returned
promise resolves once the server has registered the process, so `writeStdin` is
safe to call immediately. Pass `tty: true` for an interactive PTY (stderr is
merged into stdout, and a `CSI 8` sequence written to stdin resizes the PTY).

```ts
const exec = await sandbox.execStream("node", { cwd: "/workspace", tty: true });

await exec.writeStdin("console.log('the answer is', 6 * 7)\r");
await exec.writeStdin(".exit\r");

for await (const pipe of exec.pipes) {
  if (pipe.stdout) process.stdout.write(pipe.stdout);
  if (pipe.stderr) process.stderr.write(pipe.stderr);
}

console.log("exit code:", await exec.exitCode);
```

**`ExecStreamOptions`**: `cwd`, `env`, `tty`, `signal`, `timeoutMs`.

#### `sandbox.getEventsStream(opts?)`

Long-lived async iterator over `SandboxEvent`s for this sandbox. Auto-resumes
across disconnects — if the SSE connection drops the iterator silently
reconnects with the last observed id, so no events are missed.

```ts
const ac = new AbortController();

for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  if (event.type === "stdio") {
    process.stdout.write(event.stdout ?? "");
  }
}
```

**`EventsStreamOptions`**

| Field         | Type          | Default | Description                                       |
| ------------- | ------------- | ------- | ------------------------------------------------- |
| `signal`      | `AbortSignal` | —       | Abort the stream from the caller's side           |
| `lastEventId` | `number`      | —       | Skip past this id on the first connect            |
| `maxRetries`  | `number`      | `3`     | Max reconnect attempts after a dropped connection |

Event types include `stdio`, `exec.request` / `exec.response`,
`egress.request` / `egress.response` / `egress.chunk`,
`fs.request` / `fs.response`, `ingress.request` / `ingress.response`,
`config.apply`, and `resource.usage`.

#### `sandbox.listDirectory(path)`

Lists the immediate children of a directory under a sandbox mount. `path` is the
agent-visible absolute path. Returns one entry per child with `name`, `path`,
`is_dir`, and `size`. Like the other file calls, it bypasses per-mount ACLs.

```ts
const entries = await sandbox.listDirectory("/workspace");
for (const e of entries) {
  console.log(e.is_dir ? "dir " : "file", e.size, e.path);
}
```

#### `sandbox.uploadFile(path, content)`

Uploads a file to a sandbox mount. `path` is the agent-visible absolute path
(e.g. `/workspace/data.csv`) and must resolve beneath a configured
`fs[].mount`. Returns `{ path, bytes }`.

```ts
const { path, bytes } = await sandbox.uploadFile(
  "/workspace/data.csv",
  csvContent, // string | Uint8Array | ArrayBuffer | Blob
);
console.log(`uploaded ${bytes} bytes → ${path}`);
```

#### `sandbox.downloadFile(path)`

Downloads a file from a sandbox mount by its agent-visible absolute path.
Returns `Uint8Array`.

```ts
const bytes = await sandbox.downloadFile("/workspace/output.json");
const text = new TextDecoder().decode(bytes);
```

---

### Sandbox config

`SandboxConfig` describes the desired state of a sandbox. All fields are
optional.

| Field        | Type                     | Default            | Notes                                                                                                     |
| ------------ | ------------------------ | ------------------ | --------------------------------------------------------------------------------------------------------- |
| `image`      | `string`                 | —                  | Agent image to launch. Immutable after init. Determines isolation (a microvm image ships a guest rootfs). |
| `cpu`        | `number`                 | `1`                | Virtual CPUs. Immutable after init.                                                                       |
| `memory`     | `number`                 | `512`              | Memory in MiB. Immutable after init.                                                                      |
| `entrypoint` | `string \| string[]`     | image default      | Override the container entrypoint (argv array or whitespace-split string).                                |
| `env`        | `Record<string, string>` | —                  | Extra environment variables. Immutable after init.                                                        |
| `ttl`        | `number`                 | `1800`             | Idle TTL in seconds. Reset with `ping()`. `0` disables shutdown.                                          |
| `fs`         | `FileSystem[]`           | local `/workspace` | See [Filesystems](#filesystems).                                                                          |
| `egress`     | `EgressRule[]`           | allow-all          | Ordered rules; see [Egress](#egress-rules--overrides).                                                    |
| `snapshot`   | `Snapshot`               | —                  | See [Snapshots](#snapshots).                                                                              |

---

### Egress rules & overrides

`egress` is an **ordered list** of rules. The first rule that matches a request
decides the outcome; requests that match no rule are denied. Each rule sets
`access: "allow" | "deny"` plus matchers (`host`, `ports`, `methods`, `paths`).

```ts
egress: [
  {
    access: "allow",
    host: "api.github.com",
    methods: ["GET"],
    paths: ["/repos/*"],
  },
  { access: "deny", host: "*" }, // catch-all
];
```

#### Overrides — inject secrets the agent never sees

The `override` field on an `allow` rule injects values into every matching
outbound request before it leaves the sandbox. This is the recommended way to
keep API keys out of agent-visible environment variables or command output. If
the agent already set the same header/query parameter, the proxy overwrites it;
the agent cannot read the injected values back.

```ts
const sandbox = await hiver.getOrCreateSandbox("my-sandbox", {
  egress: [
    {
      access: "allow",
      host: "api.example.com",
      paths: ["/v1/*"],
      override: {
        headers: { Authorization: "Bearer sk-live-abc123" },
        query: { api_key: "sk-live-abc123" },
      },
    },
  ],
});
```

The agent can call `curl https://api.example.com/v1/data` with no credentials —
Hiver appends the `Authorization` header and `?api_key=…` transparently.
`headers` and `query` may be used independently or together.

---

### Filesystems

The `fs` array configures one or more mounts visible inside the sandbox. Mount
paths must be unique and non-overlapping. Access is governed by `acls`,
evaluated longest-prefix-first with deny as the default.

#### Local

```ts
fs: [
  {
    backend: "local",
    mount: "/workspace",
    acls: [{ path: "/workspace/**", access: "rw" }],
    origin: "./local-dir", // optional; Docker runtime only — see below
  },
];
```

Files live only for the lifetime of the sandbox (unless captured via a
[snapshot](#snapshots)). The optional `origin` mounts a host directory into the
sandbox — handy in local development, e.g. mounting an agent's skills directory
so edits show up live. Local origins are only supported with the Docker runtime.

#### Google Drive

Mount a Google Drive folder so every file the agent writes is persisted to Drive
and survives sandbox restarts.

```ts
fs: [
  {
    backend: "gdrive",
    mount: "/workspace",
    acls: [{ path: "/workspace/**", access: "rw" }],
    gdrive_access_token: "<oauth-access-token>",
    gdrive_refresh_token: "<oauth-refresh-token>",
    gdrive_client_id: "<google-client-id>",
    gdrive_client_secret: "<google-client-secret>",
    gdrive_folder_id: "<drive-folder-id>",
  },
];
```

**OAuth tokens** — obtain via the [Google OAuth 2.0 flow](https://developers.google.com/identity/protocols/oauth2)
with the `https://www.googleapis.com/auth/drive` scope. `gdrive_refresh_token`
is used to renew the access token automatically. Alternatively, supply
`gdrive_service_account_json` instead of the OAuth fields.

**`gdrive_folder_id`** — the ID of the Drive folder to expose as the mount root.
Find it in the folder's URL: `https://drive.google.com/drive/folders/<folder-id>`.
When omitted, the account root is used.

#### Google Cloud Storage

Mount a GCS bucket (or a prefix within one) so files persist beyond sandbox
lifetime.

```ts
fs: [
  {
    backend: "gcs",
    mount: "/workspace",
    acls: [{ path: "/workspace/**", access: "rw" }],
    gcs_bucket: "<bucket-name>",
    gcs_prefix: "optional/key/prefix", // optional
    gcs_service_account_json: JSON.stringify(serviceAccountKey),
  },
];
```

**`gcs_bucket`** — the GCS bucket name (required).

**`gcs_prefix`** — an optional key prefix that scopes the mount to a sub-path
within the bucket.

**`gcs_service_account_json`** — service account credential JSON.

---

### Snapshots

A snapshot has two independent parts: `files` captures part of the sandbox's
filesystem (restored before the next start — even for paths outside a host-backed
mount — and, with `write_on_shutdown`, captured automatically before shutdown),
and `vm` captures the full microVM state (a no-op on container isolation).
Configure it with `snapshot`:

```ts
const config: hiver.SandboxConfig = {
  image: "node",
  snapshot: {
    files: {
      key: "session-42", // restored on start, written on shutdown
      write_on_shutdown: true, // capture on shutdown (else use sandbox.snapshot())
      include: ["/root/**"], // glob paths to capture
    },
  },
};
```

Boot the sandbox under the same `files.key` later to bring the captured files
back, or capture on demand without stopping the sandbox with
`await sandbox.snapshot({ files: { key: "session-42" } })`.

---

### Utils

#### `allowedPythonPackages`

Generates the egress rules needed to let the sandbox install specific Python
packages via pip.

```ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("my-sandbox", {
  egress: [...hiver.allowedPythonPackages("numpy", "pandas", "matplotlib")],
});
```

Only the packages you name are allowed through — any `pip install` for an
unlisted package is blocked by the egress policy.

#### `allowedNpmPackages`

Generates the egress rules needed to let the sandbox install specific NPM
packages.

```ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("my-sandbox", {
  egress: [...hiver.allowedNpmPackages("lodash")],
});
```

Only the packages you name are allowed through — any `npm install` for an
unlisted package is blocked by the egress policy.

#### `allowSandbox`

Generates the egress rules that let an agent create and reach a single nested
sandbox named `sandboxKey` through the gateway, using a fixed `config` the agent
cannot tamper with. The `POST` that creates the sandbox has its request body
replaced with `config` (so the agent cannot influence what gets created), and
the nested sandbox's proxy routes are allowed through.

```ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("orchestrator", {
  egress: [
    ...hiver.allowSandbox("worker-1", {
      agent: { image: "my-agent:latest" },
    }),
  ],
});
```

Pass an optional `allowedDirs` array to also grant the agent file access to
specific directories in the nested sandbox. Each entry opens the file API under
that directory — `POST`/`GET`/`DELETE` on
`/v1/{key}/file/<dir>/**` — so the agent can seed and read back files there
without being able to touch the rest of the sandbox's filesystem.

```ts
const sandbox = await hiver.getOrCreateSandbox("orchestrator", {
  egress: [
    ...hiver.allowSandbox(
      "worker-1",
      { agent: { image: "my-agent:latest" } },
      ["workspace/inputs", "workspace/outputs"],
    ),
  ],
});
```

Add the returned rules to the outer `SandboxConfig.egress`.

---

## 🖥️ Exec

There are two ways to run code in a sandbox.

**One-shot** — `exec()` runs a command, waits for it to finish, and returns the
buffered output. Best for quick, self-contained commands.

```ts
const { stdout } = await sandbox.exec('node -e "console.log(6 * 7)"');
console.log(stdout.trim()); // 42
```

**Sessions** — `execStream()` keeps a process alive: you stream its output via
`exec.pipes` and feed it more input via `exec.writeStdin()`. Because the process
stays running, its in-memory state persists across writes — so expensive setup
(a launched browser, a loaded model, an open DB connection) happens **once** and
is reused by every later command.

The simplest version of this is an interactive interpreter. Here a single Python
session imports a library once, then reuses it across several writes while
keeping a running total in memory:

```ts
const exec = await sandbox.execStream(["python3", "-iq"], {
  cwd: "/workspace",
});

const commands = [
  "import math; total = 0", // setup runs once...
  "total += math.factorial(5); print(total)", // ...and is reused
  "total += math.factorial(6); print(total)",
  "exit()",
];
for (const cmd of commands) await exec.writeStdin(cmd + "\n");

for await (const pipe of exec.pipes) {
  if (pipe.stdout) process.stdout.write(pipe.stdout); // 120, then 840
}
```

Same idea, bigger payoff: the [`browser-cdp`](https://github.com/hiver-sh/examples/tree/main/browser-cdp/typescript)
example attaches to a resident Chromium once and reuses that one browser to scrape
multiple pages, instead of paying the browser startup cost per scrape. Pass
`tty: true` for a real PTY (stderr merges into stdout, and a `CSI 8` sequence on
stdin resizes it) — see [`terminal-attach`](https://github.com/hiver-sh/examples/tree/main/terminal-attach/typescript).

See [`sandbox.exec()`](#sandboxexeccommand-opts) and
[`sandbox.execStream()`](#sandboxexecstreamcommand-opts) for the full options.

---

### Claude agent with a Google Drive filesystem

The agent uses a Swagger spec to discover endpoints, then the sandbox uses cURL
to fetch data, Python to build financial models, and Google Drive to persist
files over a FUSE mount.

Hiver keeps the API tokens secure and the generated files persisted to Google
Drive — all while the agent only uses basic Bash commands. The next time the
agent runs, the files are already there to be reused, saving tokens.

See [`claude-agent-gdrive-filesystem`](https://github.com/hiver-sh/examples/tree/main/claude-agent-gdrive-filesystem/typescript)
(and the local-filesystem variant, [`claude-agent`](https://github.com/hiver-sh/examples/tree/main/claude-agent/typescript)).
