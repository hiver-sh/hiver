# Hive TypeScript Client

TypeScript client for Hive sandboxes. Works in Node.js (≥18) and Bun.

## Installation

```sh
npm install hive
```

## Quick start

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

console.log("sandbox URL:", sandbox.url);

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

## API

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

---

## Examples

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

### Event streaming

Build a Docker image, provision a sandbox from it, and consume all events until the container exits.

```sh
npx tsx examples/events.ts
```

### Claude Agent

A Claude agent running inside a Hive sandbox, with a financial data API injected via egress overrides and files persisted to a network filesystem.

```sh
ANTHROPIC_API_KEY='<token>' FINNHUB_API_KEY='<token>' npx tsx examples/claude-agent.ts
```
