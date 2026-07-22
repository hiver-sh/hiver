<p align="center">
<img src="./docs/hive.svg" width="100">
</p>

<h1 align="center">Hiver</h1>

<h3 align="center">
Replayable Sandboxes for AI Agents
</h3>

<p align="center">
<img src="./docs/replay.gif" alt="Replaying an agent run in the Hiver inspector">
</p>

Improving agent performance requires a mini-RL loop: run, evaluate, update policy, repeat. Unfortunately, this isn’t very straightforward as agents have become stateful, distributed systems. Turning behavior into evals isn’t always simple as every state mutation to the environment makes the reproduction much harder.

Hiver combines the sandbox and observability, so every agent run can be turned into insights to improve future runs.

It’s composed of a secure sandbox that works locally and in the cloud with different levels of isolation, an inspector web interface with live telemetry and a CLI that is used by a coding agent to improve the agent itself.

Hiver doesn’t require an API key or a specific cloud service.

## 🚀 Getting Started

Install the Hiver CLI:

```sh
npm install --global @hiver.sh/cli && hiver

# If you don't have NPM:
curl -fsSL https://hiver.sh/install | sh
```

Installing the CLI also installs the `/hiver` skill, which gives coding agents the documentation needed to drive the CLI and client SDK.

Then bring up the local stack, start an agent from a built-in image, and open the inspector — three commands, no config:

```sh
hiver up                # start the local gateway
hiver start agent-1     # launch Claude Code, Codex, Copilot, or Gemini in an isolated sandbox
hiver inspect agent-1   # open the inspector and replay every file, request, and tool call
```

That's the full loop: an agent running in an isolated sandbox, with a live, replayable record of everything it does.

The CLI manages sandboxes, streams live events, and launches the inspector:

```sh
⬢ Hiver · Agent Runtime

  Usage: hiver <command> [options]

  Commands
    up             Bring up local stack
    down           Bring down local stack
    connect        Connect to stack
    start          Start a sandbox
    run            Build and launch a project directory as a sandbox
    stop           Stop a sandbox
    shell          Open an interactive shell in a sandbox
    list           List the sandboxes
    events         Stream a sandbox's events live as they happen
    inspect        Launch the inspector
    bundle         Add runtime to OCI image

  Run hiver <command> --help for command details.
```

### Built-in images

Hiver ships ready-to-run images for either containers or microvms:

| Name         | Description                                                                                                                |
| ------------ | -------------------------------------------------------------------------------------------------------------------------- |
| `claude`     | [Claude Code](https://github.com/anthropics/claude-code) (`@anthropic-ai/claude-code`) installed.                         |
| `codex`      | [Codex](https://github.com/openai/codex) (`@openai/codex`) installed.                                                      |
| `copilot`    | [GitHub Copilot CLI](https://github.com/github/copilot) (`@github/copilot`) installed.                                     |
| `openclaw`   | [OpenClaw](https://github.com/openclaw/openclaw) (`openclaw`) installed.                                                    |
| `browser`    | Chromium with CDP (Chrome DevTools Protocol) exposed for agentic web navigation.                                           |
| `node`       | Minimal Node.js runtime for running JavaScript/TypeScript workloads.                                                       |
| `python`     | Minimal Python 3.13 runtime for running Python workloads.                                                                  |

Bring your own image too — bundle any Docker image with `hiver bundle` (see [Isolation](#isolation)) and point a sandbox at it.

## Documentation

- [Hiver Docs](https://hiver.sh/docs)

## Examples

Find [**runnable examples**](https://github.com/hiver-sh/examples) in TypeScript and Python — Agent SDK servers, CLI and browser drivers, and lower-level client SDK recipes.

**⭐ [Open Work](https://github.com/hiver-sh/work)** — a full Next.js app demonstrating a complete agent with security, elicitation, content collaboration with AI and a Web browser.

### Run a project directory

`hiver run` bundles a directory that contains a `Dockerfile` and a `.hiver.json`, then launches it as a sandbox in one step — no separate `hiver bundle`. This is the pattern for **Agent SDK servers**, where the agent loop runs _inside_ the sandbox as an HTTP service.

A minimal project looks like:

```jsonc
// .hiver.json — the image tag plus an egress policy. The API key is injected by
// the proxy via `override`, so it never lives in the sandbox's env or context.
{
  "image": "my-agent",
  "egress": [
    {
      "access": "allow",
      "host": "api.anthropic.com",
      "override": { "headers": { "x-api-key": "sk-ant-..." } }
    },
    { "access": "deny", "host": "*" }
  ]
}
```

Launch it in one command:

```sh
hiver run . my-agent
```

Then drive the in-sandbox server over the gateway:

```ts
import { getOrCreateSandbox } from "@hiver.sh/client";

const sandbox = await getOrCreateSandbox("my-agent", { image: "my-agent" });
const res = await fetch(`${sandbox.proxyUrl(3000)}chat`, {
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify({ prompt: "Create /workspace/fib.py and run it." }),
});
console.log((await res.json()).reply);
```

See the [Claude Agent SDK example](https://github.com/hiver-sh/examples/tree/main/claude-agent-sdk) for a complete, runnable version.

## Inspector

The inspector is the fastest way to understand what your agent is _actually_ doing. Launch it with a single command:

```sh
hiver inspect
```

It opens a live, DevTools-style UI over a running sandbox. In one place you get:

- **Timeline**: a waterfall of every event the agent generates, laid out over time with per-request durations. Click any bar to inspect the full request and response (headers, body, and verdict) and see exactly where the run spent its time.
- **LLM**: the inspector decodes the model traffic itself and renders the conversation as **system**, **user**, and **assistant** messages, tool calls and all, with no agent-side hooks required. Built-in decoders cover Claude Code / Anthropic, Codex / ChatGPT, and Gemini, and the provider interface is pluggable, so GitHub Copilot, OpenClaw, or your own CLI drop in with a few lines.
- **Network**: every egress request the agent makes, with the host, path, and whether it was **allowed** or **denied** by policy. TLS is decrypted transparently, so you see the real requests, not opaque CONNECT tunnels.
- **Files**: every read and write the agent performs, across local, Google Drive, GCS, S3, and other backends, with the same allowed/denied verdicts.
- **Exec**: the commands the agent runs and their output.
- **Terminal**: drop into an interactive shell inside the sandbox to poke around mid-run.
- **Browser**: a live view of the agent's browser session, so you can watch it navigate pages in real time.
- **Config**: view and edit the sandbox's network and filesystem policy live, then watch the agent react.

Because the inspector is just a client over the same event stream and API the SDKs use, anything you see in the UI you can also script.

## Events

Every sandbox emits a structured, ordered, replayable stream of audit events. Stream them live from the CLI:

```sh
hiver events agent-1 --follow
```

Then, pipe the event backlog into an LLM to get a plain-English summary of what the agent did:

```sh
hiver events agent-1 \
  | jq -c 'select(.type | IN("exec.request","egress.request","fs.request","stdio"))' \
  | claude -p "what did the agent do?"
```

## Isolation

Every sandbox runs the agent as an untrusted workload, but you can pick _how_ it's confined. Both backends sit behind the same API, file systems, ACLs, egress policy, and audit stream; only the boundary around the workload differs:

- **Container** (the default): the lightest option, fastest to start and lowest overhead, ideal for development and trusted images.

- **MicroVM**: runs the agent behind a hardware-virtualization boundary with its own kernel, the stronger choice for fully untrusted code. Requires KVM on the host; in a virtualized environment (cloud VM, CI runner) that means **nested virtualization** must be enabled.

You choose which one you get when you bundle your Docker image into a Hiver runtime image with `hiver bundle`:

```sh
# Container isolation — the default.
hiver bundle my-agent:latest --tag my-agent-runtime

# MicroVM isolation.
hiver bundle my-agent:latest --tag my-agent-runtime --microvm

# Build for multiple architectures and push to the registry.
hiver bundle my-agent:latest --tag my-agent-runtime \
  --platform linux/amd64,linux/arm64 --push
```

Point a sandbox at the bundled image and the runtime boots it under the matching backend. Read back which one it chose via `GetInfo`:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "my-agent-runtime",
});
const info = await sandbox.getInfo();
console.log(info.isolation); // "container" or "microvm"
```

## API overview

### Exec

Run commands inside the sandbox, from a single shell command to a long-running process, with every invocation audited as an `exec.request` event. There are two execution modes:

**One-shot**: `exec()` runs a command, waits for it to finish, and returns the buffered output:

```ts
const { stdout } = await sandbox.exec(["node", "-e", "console.log(6 * 7)"]);
console.log(stdout.trim()); // 42
```

**Sessions**: `execStream()` keeps a process alive: you stream its output via `exec.pipes` and feed it more input via `exec.writeStdin()`. Because the process stays running, its in-memory state persists across writes, so expensive setup (a launched browser, a loaded model, an open DB connection) happens **once** and is reused by every later command.

The simplest version is an interactive interpreter:

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

### FUSE File System

FUSE is a first-class citizen in Hiver. A sandbox's filesystem is assembled from mounts you declare.

#### Cloud storage backends

A mount can be backed by **local** storage, **GCS**, **S3**, **Azure Blob Storage**, **Google Drive**, or **OneDrive**. Each takes the same `mount` and `acls` fields; only the credential fields differ by backend:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "node",
  fs: [
    // The user's working directory: writable, but secrets are off-limits.
    {
      backend: "local",
      mount: "/workspace",
      acls: [
        { path: "/workspace", access: "rw" },
        { path: "/workspace/.env", access: "deny" },
      ],
    },
    // Org knowledge base, mounted from a shared bucket (read-only).
    {
      backend: "gcs",
      mount: "/knowledge",
      gcs_bucket: "acme-handbook",
      gcs_service_account_json: SA_JSON,
      acls: [{ path: "/knowledge", access: "ro" }],
    },
    // Amazon S3 (or any S3-compatible service via `s3_endpoint`).
    {
      backend: "s3",
      mount: "/s3",
      s3_bucket: "acme-data",
      s3_region: "us-east-1",
      s3_access_key_id: AWS_ACCESS_KEY_ID,
      s3_secret_access_key: AWS_SECRET_ACCESS_KEY,
      acls: [{ path: "/s3", access: "rw" }],
    },
    // Azure Blob Storage.
    {
      backend: "azure",
      mount: "/azure",
      azure_account: "acmestorage",
      azure_container: "agent-data",
      azure_account_key: AZURE_STORAGE_KEY,
      acls: [{ path: "/azure", access: "rw" }],
    },
    // Google Drive (a shared folder, via a service account).
    {
      backend: "gdrive",
      mount: "/drive",
      gdrive_folder_id: "1AbCdEfGhIjKlMnOpQrStUvWxYz",
      gdrive_service_account_json: SA_JSON,
      acls: [{ path: "/drive", access: "ro" }],
    },
    // OneDrive.
    {
      backend: "onedrive",
      mount: "/onedrive",
      onedrive_access_token: ONEDRIVE_ACCESS_TOKEN,
      acls: [{ path: "/onedrive", access: "rw" }],
    },
  ],
});
```

Each backend takes an optional `*_prefix` to scope the mount to a subpath of the bucket, container, or drive. Credentials can also be supplied through the runtime's environment instead of the config, so they never appear in the sandbox spec.

#### Network File System

Point a mount at any HTTP host that implements the [file system interface](api/external_file_system.yaml) and the runtime turns every read and write into a call against it, letting you bring your own storage without a built-in backend:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "node",
  fs: [
    { backend: "external", mount: "/data", host: "https://fs.internal:8080" },
  ],
});
```

### Snapshots

A snapshot is the sandbox's working tree captured to a tarball on shutdown and restored on start. Point `snapshot.mount` at an **internal**, remote-backed file system and the tarball is persisted to blob storage instead of local disk, so a fresh sandbox can resume from where a previous one left off:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "node",
  fs: [
    // The snapshot target, backed by GCS. `internal` keeps it out of the
    // agent's view — it exists only for the runtime to read/write snapshots.
    {
      backend: "gcs",
      mount: "/snapshots",
      internal: true,
      gcs_bucket: "acme-snapshots",
      gcs_service_account_json: SA_JSON,
    },
  ],
  snapshot: {
    files: {
      key: "agent-1", // tarball to restore on start / write on shutdown
      write_on_shutdown: true, // capture before the sandbox stops
      mount: "/snapshots", // pull it from the GCS-backed mount
      include: ["/workspace/**"], // paths to capture
    },
  },
});
```

#### VM state

On the microVM backend, `snapshot.vm` captures the sandbox's full CPU and memory state, not just its files:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "my-agent-runtime", // a microVM-isolated image
  snapshot: {
    // Resume from this VM snapshot if it exists; otherwise cold-boot.
    vm: { key: "agent-1" },
  },
});

// Capture (or refresh) the VM snapshot without stopping the sandbox.
await sandbox.snapshot({ vm: { key: "agent-1" } });
```

`vm` and `files` are independent parts, so you can capture both at once to resume a sandbox with its in-memory state and its writable filesystem together.

### Files

Move files in and out of a sandbox. Operations bypass the mount's ACLs; the API is a higher-privilege control surface than the workload.

```ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "node",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/**", access: "rw" }],
    },
  ],
});

// Seed an input file before the agent runs.
await sandbox.writeFile("/workspace", "data.csv", csvContent);

// List a directory to see what the agent produced.
const entries = await sandbox.listDirectory("/workspace");
// entries: [{ name, path, is_dir, size }, ...]

// Pull a specific file out of the sandbox.
const report = await sandbox.readFile("/workspace/report.pdf");

// Remove a file.
await sandbox.deleteFile("/workspace/data.csv");
```

### Request Overrides

The egress proxy can **rewrite** requests on the way out. Attach an `override` to an egress rule and the proxy can inject **headers** and **query parameters**, redirect the request to a different **upstream host**, and prepend a **path prefix**, all on every matching request, overwriting whatever the agent set:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "node",
  egress: [
    {
      access: "allow",
      host: "api.internal.acme.com",
      override: {
        // Auth token injected as a header. The agent never sees it.
        headers: { Authorization: `Bearer ${API_TOKEN}` },
        // URL params the proxy stamps onto the request.
        query: { tenant: "acme", "api-version": "2024-01" },
      },
    },
  ],
});
```

#### Redirecting to a mock server

`host` and `prefix_path` rewrite _where_ the request goes, so you can transparently point an agent at a local mock or stub server without touching its code:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "node",
  egress: [
    {
      access: "allow",
      host: "api.openai.com",
      override: {
        // Dial a local mock instead of the real API...
        host: "mockserver.internal:8080",
        // ...and namespace its routes under /openai.
        // The agent's GET /v1/models arrives as GET /openai/v1/models.
        prefix_path: "/openai",
      },
    },
  ],
});
```

The agent issues a normal `https://api.openai.com/v1/models` request; the proxy dials `mockserver.internal:8080` and forwards it as `/openai/v1/models`.

#### Lua script

Use `override_script` to run a Lua script on each matching request, after the declarative `override` is applied. The script can read `method`, `host`, `path`, and `query`, and can mutate `body` and `headers` to rewrite the request:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  image: "node",
  egress: [
    {
      access: "allow",
      host: "api.internal.acme.com",
      // Redact an account number from every request body on the way out.
      override_script: `
        body = string.gsub(body, "acct_%d+", "acct_[redacted]")
        headers["x-rewritten-by"] = "hiver"
      `,
    },
  ],
});
```

### Events

Subscribe to a sandbox's audit stream from any client and react to events as they happen:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {"image": "node"});

for await (const event of sandbox.getEventsStream()) {
  console.log(event.type, event.access ?? "");
}
```

Each event is structured, ordered, and tagged with whether the action was allowed or denied:

```txt
exec.request    allowed
egress.request  allowed
fs.request      denied
stdio
```

## Cloud Deployment

Run the full stack (controller, gateway, and sandboxes) on a managed Kubernetes cluster, and drive it from the same client library you use locally.

Deployment is split into two pieces: provisioning a cluster, and installing the control plane onto it.

### Google Kubernetes Engine (GKE)

[`deployment/gke`](deployment/gke/README.md) provisions a GKE cluster with Terraform. The node pool is configured for microVM isolation out of the box: nested virtualization is enabled so KVM works inside the nodes, and nodes are backed by Local SSD for fast snapshot and prewarm I/O. See the [GKE deployment guide](deployment/gke/README.md) for prerequisites and `terraform apply` steps.

### Helm Chart

[`deployment/k8s/chart`](deployment/k8s/chart/README.md) is a Helm chart that deploys the controller, the Envoy gateway, and the per-service sandbox pools onto any Kubernetes cluster.


### Architecture

The Hiver runtime runs inside a container and is composed of sidecar processes. The agent sandbox runs on `runc` or `firecracker` as an untrusted workload. `sbxfuse` provides FUSE-backed volumes, `sbxproxy` transparently intercepts all TCP traffic (including TLS), and `sandboxd` wires everything together, serving the client API, reconciling sidecar policy, and streaming telemetry events.

<p align="center">
<img src="./docs/hiver-arch.svg" width="500">
</p>

The root filesystem is assembled with overlayfs, layering the agent's writes over the read-only base image for efficient snapshotting.

A typical deployment also includes a controller for sandbox lifecycle management and an Envoy gateway for external network access. All components ship out of the box but can be swapped for custom implementations.

Sandboxes don't get a pod each. They are packed into a host pod and share its `sandboxd`, `sbxfuse`, and `sbxproxy` sidecars, so a new sandbox starts in a process rather than a cold pod. Placement picks a host that already serves the requested image, still has resource headroom under its limits, and satisfies the node constraints for that image (for example, microVM isolation requires nodes with nested virtualization).

Hiver is unopinionated about orchestration: the agent CLI or SDK can run entirely inside the sandbox or in a separate deployment. Because everything inside the sandbox is treated as untrusted, agents can call private APIs and access files without ever seeing auth tokens or secrets.

## Status

Hiver is under active development. The local runtime, inspector, audit stream, filesystem ACLs, and egress controls are usable today; APIs and deployment manifests may change before v1.

## License

Apache 2.0
