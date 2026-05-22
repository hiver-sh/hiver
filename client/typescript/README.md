# Hive TypeScript Client

TypeScript client for Hive sandboxes. Works in Node.js (≥18) and Bun.

## Contents

- [📦 Installation](#-installation)
- [⚡ Quick start](#-quick-start)
- [📖 API](#-api)
  - [`getOrCreateSandbox`](#getorCreatesandboxid-config-opts)
  - [`shutdown`](#shutdownsandbox)
  - [`Sandbox`](#sandbox)
    - [`sandbox.ping()`](#sandboxping)
    - [`sandbox.getConfig()`](#sandboxgetconfig)
    - [`sandbox.applyConfig()`](#sandboxapplyconfigconfig)
    - [`sandbox.getEventsStream()`](#sandboxgeteventsstreamopts-)
    - [`sandbox.uploadFile()`](#sandboxuploadfiledestination-filename-content)
    - [`sandbox.downloadFile()`](#sandboxdownloadfilepath)
  - [Egress overrides](#egress-overrides)
  - [Filesystems](#filesystems)
  - [Utils](#utils)
- [🧪 Examples](#-examples)

## 📦 Installation

```sh
npm install hive
```

## ⚡ Quick start

```ts
import * as hive from "hive";

const sandbox = await hive.getOrCreateSandbox("my-sandbox", {
  image: "mcp-server",
  ttl: 1800,
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  egress: {
    allow: [{ host: "api.github.com", methods: ["GET"], paths: ["/repos/*"] }],
  },
});

console.log("sandbox endpoint:", sandbox.exposedEndpoint);

// Keep the sandbox alive.
const ping = setInterval(sandbox.ping, 10_000);

// Stream events until done.
const ac = new AbortController();
for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  console.log("event", event);
}

clearInterval(ping);
await hive.shutdown(sandbox);
```

## 📖 API

### `getOrCreateSandbox(id, config, opts?)`

Provisions a sandbox idempotently. If a sandbox with `id` already exists it is returned unchanged and `config` is ignored; otherwise a new sandbox is created from `config`.

`id` must match `[A-Za-z0-9_-]{1,64}`.

Returns a `Sandbox` handle once the sandbox is ready to accept requests.

```ts
const sandbox = await hive.getOrCreateSandbox("my-sandbox", config, {
  controllerUrl: "http://localhost:9000", // default
  readinessTimeoutMs: 30_000, // default; pass 0 to skip readiness wait
});
```

**`ControllerOptions`**

| Field                | Type           | Default                 | Description                                                                          |
| -------------------- | -------------- | ----------------------- | ------------------------------------------------------------------------------------ |
| `controllerUrl`      | `string`       | `http://localhost:9000` | URL of the Hive controller                                                           |
| `fetch`              | `typeof fetch` | global `fetch`          | Override for testing or custom transports                                            |
| `readinessTimeoutMs` | `number`       | `30000`                 | How long to wait for the sandbox to become ready, in milliseconds. Pass `0` to skip. |

---

### `shutdown(sandbox)`

Stops the sandbox container and removes it.

```ts
await hive.shutdown(sandbox);
```

---

### `Sandbox`

Returned by `getOrCreateSandbox`. Not constructed directly.

#### Properties

| Property        | Type     | Description                                                             |
| --------------- | -------- | ----------------------------------------------------------------------- |
| `id`            | `string` | Sandbox identifier                                                      |
| `url`           | `string` | URL of the HTTP service the sandbox image exposes (first `EXPOSE` port) |
| `controllerUrl` | `string` | URL of the controller that created this sandbox                         |

#### `sandbox.ping()`

Resets the sandbox TTL countdown. Bound as an arrow function so `setInterval(sandbox.ping, 10_000)` works without `.bind`.

```ts
await sandbox.ping();
// or
const interval = setInterval(sandbox.ping, 10_000);
```

#### `sandbox.getConfig()`

Returns the current `SandboxConfig`.

```ts
const config = await sandbox.getConfig();
```

#### `sandbox.applyConfig(config)`

Applies a desired `SandboxConfig`. The server diffs it against the running state; returns an `ApplyResult` whose `applied` field indicates whether the change was committed or rolled back.

```ts
const current = await sandbox.getConfig();
const result = await sandbox.applyConfig({
  ...current,
  egress: {
    allow: [
      ...(current.egress?.allow ?? []),
      { host: "api.github.com", methods: ["GET"], paths: ["/repos/*"] },
    ],
  },
});

if (!result.applied) {
  console.error("rolled back:", result.error);
} else {
  console.log("changes:", result.changes);
}
```

#### `sandbox.getEventsStream(opts?)`

Long-lived async iterator over `SandboxEvent`s. Auto-resumes across disconnects — if the SSE connection drops the iterator silently reconnects with the last observed id, so no events are missed.

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

#### `sandbox.uploadFile(destination, filename, content)`

Uploads a file to a sandbox mount. `destination` must match a configured `fs[].mount`. Returns `{ path, bytes }`.

```ts
const { path, bytes } = await sandbox.uploadFile(
  "/workspace",
  "data.csv",
  csvContent, // string | Uint8Array | ArrayBuffer | Blob
);
console.log(`uploaded ${bytes} bytes → ${path}`);
```

#### `sandbox.downloadFile(path)`

Downloads a file from a sandbox mount by its agent-visible absolute path. Returns `Uint8Array`.

```ts
const bytes = await sandbox.downloadFile("/workspace/output.json");
const text = new TextDecoder().decode(bytes);
```

### Egress overrides

The `override` field on an egress rule injects values into every outbound request that matches the rule — before the request leaves the sandbox. This is the recommended way to keep API keys out of agent-visible environment variables or command output.

#### Inject a token as a request header

```ts
const sandbox = await hive.getOrCreateSandbox("my-sandbox", {
  egress: {
    allow: [
      {
        host: "api.example.com",
        paths: ["/v1/*"],
        override: {
          headers: {
            Authorization: "Bearer sk-live-abc123",
          },
        },
      },
    ],
  },
});
```

The agent can call `curl https://api.example.com/v1/data` with no credentials — Hive appends the `Authorization` header transparently.

#### Inject a token as a query parameter

```ts
const sandbox = await hive.getOrCreateSandbox("my-sandbox", {
  egress: {
    allow: [
      {
        host: "api.example.com",
        paths: ["/v1/*"],
        override: {
          query: {
            api_key: "sk-live-abc123",
          },
        },
      },
    ],
  },
});
```

The agent calls `curl https://api.example.com/v1/data` and Hive appends `?api_key=sk-live-abc123` automatically.

#### Both at once

`headers` and `query` can be combined in a single rule:

```ts
override: {
  headers: { "X-App-Id": "my-app" },
  query:   { token: "sk-live-abc123" },
}
```

---

### Filesystems

The `fs` array configures one or more mounts visible inside the sandbox.

#### Local

```ts
fs: [
  {
    backend: "local",
    mount: "/workspace",
    acls: [{ path: "/workspace/**", access: "rw" }],
  },
];
```

Files live only for the lifetime of the sandbox.

#### Google Drive

Mount a Google Drive folder as `/workspace` so every file the agent writes is persisted to Drive and survives sandbox restarts.

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

**OAuth tokens** — obtain via the [Google OAuth 2.0 flow](https://developers.google.com/identity/protocols/oauth2) with the `https://www.googleapis.com/auth/drive` scope. `gdrive_refresh_token` is used to renew the access token automatically.

**`gdrive_folder_id`** — the ID of the Drive folder to expose as the mount root. Find it in the folder's URL: `https://drive.google.com/drive/folders/<folder-id>`.

#### Google Cloud Storage

Mount a GCS bucket (or a prefix within one) as `/workspace` so files persist beyond sandbox lifetime.

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

**`gcs_bucket`** — the GCS bucket name (required). Can also be set via `HIVE_GCS_BUCKET`.

**`gcs_prefix`** — an optional key prefix that scopes the mount to a sub-path within the bucket. Can also be set via `HIVE_GCS_PREFIX`.

**`gcs_service_account_json`** — service account credential JSON. When omitted, [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials) are used (`GOOGLE_APPLICATION_CREDENTIALS`, gcloud user credentials, or the GCE/GKE metadata server). Can also be set via `HIVE_GCS_SERVICE_ACCOUNT_JSON`.

---

### Utils

#### `allowedPythonPackages`

A helper that generates the egress rules needed to let the sandbox install specific Python packages via pip.

```ts
import * as hive from "hive";

const sandbox = await hive.getOrCreateSandbox("my-sandbox", {
  egress: {
    allow: [...hive.allowedPythonPackages("numpy", "pandas", "matplotlib")],
  },
});
```

Only the packages you name are allowed through — any `pip install` for an unlisted package will be blocked by the egress policy.

#### `allowedNpmPackages`

A helper that generates the egress rules needed to let the sandbox install specific NPM packages.

```ts
import * as hive from "hive";

const sandbox = await hive.getOrCreateSandbox("my-sandbox", {
  egress: {
    allow: [...hive.allowedNpmPackages("lodash")],
  },
});
```

Only the packages you name are allowed through — any `npm install` for an unlisted package will be blocked by the egress policy.

---

## 🧪 Examples

### Quickstart

Provision a sandbox, stream its events, and keep it alive with periodic pings.

```sh
npx tsx examples/quickstart.ts
```

### Config management

Read the current config, add an egress rule, and apply the update.

```sh
npx tsx examples/apply-config.ts
```

### File transfers

Upload a file into a sandbox mount and read it back out.

```sh
npx tsx examples/files.ts
```

### Custom Docker image

Build a Docker image, provision a sandbox from it, and consume all events until the container exits.

```sh
npx tsx examples/custom-image
```

### Claude Agent

The agent uses a Swagger spec to discover endpoints, then the sandbox uses CURL to fetch data, Python to build financial models, and Google drive to persist files over a fuse mount.

The Hive Sandbox keeps the API tokens secure and generated files persisted to Google drive all while using basic Bash commands.
The next time this agent runs, all the files are available, so they can be re-used to save tokens and increase learnings.

For example, the agent can store markdown, json files and use them next time this agent runs.

```sh
ANTHROPIC_API_KEY='<token>' \
FINNHUB_API_KEY='<token>' \
GOOGLE_CLIENT_ID='<client-id>' \
GOOGLE_CLIENT_SECRET='<client-secret>' \
npx tsx client/typescript/examples/claude-agent-gdrive-filesystem.ts
```

There's also a version of this agent with a local file system:

```sh
ANTHROPIC_API_KEY='<token>' \
FINNHUB_API_KEY='<token>' \
npx tsx client/typescript/examples/claude-agent-gdrive-filesystem.ts
```

### Local directory mount

During local development, it can be helpful to mount a local directory into the sandbox.
For example, an agent skills directory. This way, it's seamless to make changes to the skills.

```sh
npx tsx examples/local-filesystem-mount
```
