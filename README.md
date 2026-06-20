<p align="center">
<img src="./docs/hive.svg" width="100">
</p>
<h1 align="center">Hiver</h1>
<h3 align="center">Chrome DevTools for Agents</h3>

<p align="center">
Run agents autonomously with controlled network access, auditable file operations, and full execution visibility.
</p>

<p align="center">
<img src="./docs/devtools.png">
</p>

## What is Hiver?

Hiver is a runtime for running AI agents as untrusted workloads, with visibility and control over their file, network, command, and model interactions. It has two parts:

- **The runtime** boots each agent into an isolated sandbox with its own file systems, network egress policy, and path-level ACLs. Use MicroVM isolation for fully untrusted code that needs its own kernel and hardware-virtualization boundary, or lightweight container isolation for faster, trusted workloads. Both run behind the same API. Every command, file access, and network request is mediated by the runtime and emitted as a structured, replayable audit event.
- **The inspector** is a live, DevTools-style UI over a running sandbox. It decodes the agent’s LLM traffic into readable conversations, shows every egress request and file operation with its allowed/denied verdict, surfaces a timeline of activity, and lets you edit sandbox policy on the fly — all over the same event stream and API used by the SDKs.

Hiver supports both local development and cloud deployment. The same client library and runtime work whether you run hiver start on your machine or deploy to a cluster. Bring your own Docker image and run it in Hiver with no application changes.

## 🚀 Getting Started

Install the Hiver CLI:

```sh
npm install --global @hiver.sh/cli

# If you don't have NPM:
curl -fsSL https://hiver.sh/install | sh
```

Use the CLI to manage sandboxes, stream live events, and launch the inspector:

```sh
⬢ Hiver · Agent Runtime v0.1.15

  Usage: hiver <command> [options]

  Commands
    up       Bring up the stack
    down     Bring down the stack
    start    Start a sandbox
    stop     Stop a sandbox
    shell    Open an interactive shell in a sandbox
    list     List the sandboxes
    events   Stream a sandbox's events live as they happen
    inspect  Launch the inspector
    bundle   Bundle a Docker image into a Hiver runtime image

  Run hiver <command> --help for command details.
```

### Client Support

Hiver ships first-party clients for **TypeScript**, **Python**, and **Go**. Launch your first agent with any of them:

#### TypeScript

```ts
// npm install --save @hiver.sh/client
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1");
const result = await sandbox.exec(["claude", "-p", "Write a poem and save it as pdf"]);
console.log(result.stdout);
```

#### Python

```python
# pip install hiver-py
import asyncio
import hiver

async def main():
    sandbox = await hiver.get_or_create_sandbox("agent-1")
    result = await sandbox.exec(["claude", "-p", "Write a poem and save it as pdf"])
    print(result["stdout"])

asyncio.run(main())
```

#### Go

```go
// go get github.com/hiver-sh/hiver/client
import "github.com/hiver-sh/hiver/client"

c := client.NewClient("http://localhost:10000")
sandbox, _ := c.GetOrCreateSandbox(context.Background(), "agent-1", client.SandboxConfig{})

result, _ := sandbox.Exec(context.Background(),
    client.ExecRequest{Command: []string{"claude", "-p", "Write a poem and save it as pdf"} })
fmt.Println(result.Stdout)
```

## Inspector

The inspector is the fastest way to understand what your agent is *actually* doing. Launch it with a single command:

```sh
hiver inspect
```

It opens a live, DevTools-style UI over a running sandbox. In one place you get:

* **Timeline**: a waterfall of every event the agent generates, laid out over time with per-request durations. Click any bar to inspect the full request and response (headers, body, and verdict) and see exactly where the run spent its time.
* **LLM**: the inspector decodes the model traffic itself and renders the conversation as **system**, **user**, and **assistant** messages, tool calls and all, with no agent-side hooks required. Built-in decoders cover Claude Code / Anthropic, Codex / ChatGPT, and Gemini, and the provider interface is pluggable, so GitHub Copilot, OpenCode, or your own CLI drop in with a few lines.
* **Network**: every egress request the agent makes, with the host, path, and whether it was **allowed** or **denied** by policy. TLS is decrypted transparently, so you see the real requests, not opaque CONNECT tunnels.
* **Files**: every read and write the agent performs, across local, Google Drive, GCS, S3, and other backends, with the same allowed/denied verdicts.
* **Exec**: the commands the agent runs and their output.
* **Terminal**: drop into an interactive shell inside the sandbox to poke around mid-run.
* **Config**: view and edit the sandbox's network and filesystem policy live, then watch the agent react.

Because the inspector is just a client over the same event stream and API the SDKs use, anything you see in the UI you can also script.

## Events


Every sandbox emits a structured, ordered, replayable stream of audit events. Stream them live from the CLI:

```sh
hiver events agent-1 --follow
```

Then, pipe the event backlog into an LLM to get a plain-English summary of what the agent did:

```sh
hiver events claude-code-an \
  | jq -c 'select(.type | IN("exec.request","egress.request","fs.request","stdio"))' \
  | claude -p "what did the agent do?"
```

## Isolation

Every sandbox runs the agent as an untrusted workload, but you can pick *how* it's confined. Both backends sit behind the same API, file systems, ACLs, egress policy, and audit stream; only the boundary around the workload differs:

* **Container** (the default): the lightest option, fastest to start and lowest overhead, ideal for development and trusted images.

* **MicroVM**: runs the agent behind a hardware-virtualization boundary with its own kernel, the stronger choice for fully untrusted code. Requires KVM on the host; in a virtualized environment (cloud VM, CI runner) that means **nested virtualization** must be enabled.

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
const exec = await sandbox.execStream(["python3", "-iq"], { cwd: "/workspace" });

const commands = [
  "import math; total = 0",                    // setup runs once...
  "total += math.factorial(5); print(total)",  // ...and is reused
  "total += math.factorial(6); print(total)",
  "exit()",
];
for (const cmd of commands) await exec.writeStdin(cmd + "\n");

for await (const pipe of exec.pipes) {
  if (pipe.stdout) process.stdout.write(pipe.stdout); // 120, then 840
}
```

### File Systems

A sandbox's filesystem is assembled from mounts you declare, each backed by local storage, Google Drive, Google Cloud Storage, OneDrive, S3, or Azure Blob. Every mount is FUSE-backed by `sbxfuse`, so the agent sees ordinary files and directories, but every read and write passes through the runtime, where it's checked against **path-level ACLs** and emitted as an auditable `fs.request` event.

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  fs: [
    // Org knowledge base, mounted from a shared bucket (read-only).
    {
      backend: "gcs",
      mount: "/knowledge",
      gcs_bucket: "acme-handbook",
      gcs_service_account_json: SA_JSON,
      acls: [{ path: "/knowledge", access: "ro" }],
    },
    // The user's working directory: writable, but secrets are off-limits.
    {
      backend: "local",
      mount: "/workspace",
      acls: [
        { path: "/workspace", access: "rw" },
        { path: "/workspace/.env", access: "deny" },
      ],
    },
  ],
});
```

#### Network File System

Point a mount at any HTTP host that implements the [file system interface](api/external_file_system.yaml) and the runtime turns every read and write into a call against it, letting you bring your own storage without a built-in backend:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  fs: [{ backend: "external", mount: "/data", host: "https://fs.internal:8080" }],
});
```

### Snapshots

A snapshot is the sandbox's working tree captured to a tarball on shutdown and restored on start. Point `snapshot.mount` at an **internal**, remote-backed file system and the tarball is persisted to blob storage instead of local disk, so a fresh sandbox can resume from where a previous one left off:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
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
    restore_key: "agent-1",      // tarball to restore on start
    mount: "/snapshots",         // pull it from the GCS-backed mount
    include: ["/workspace/**"],  // paths to capture on shutdown
  },
});
```

### Files

The file API lets you move files in and out of a sandbox's from outside the workload. Operations bypass the mount's ACLs; the API is a higher-privilege control surface than the workload.

```ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  fs: [{ backend: "local", mount: "/workspace", acls: [{ path: "/**", access: "rw" }] }],
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

`host` and `prefix_path` rewrite *where* the request goes, so you can transparently point an agent at a local mock or stub server without touching its code:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
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

### Events

Subscribe to a sandbox's audit stream from any client and react to events as they happen:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1");

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

### GKE

[`deployment/gke`](deployment/gke/README.md) provisions a GKE cluster with Terraform and deploys the control plane to it. The node pool is configured for microVM isolation out of the box: nested virtualization is enabled so KVM works inside the nodes, and nodes are backed by Local SSD for fast snapshot and prewarm I/O. See the [GKE deployment guide](deployment/gke/README.md) for prerequisites and `terraform apply` steps.

## Full documentation

* [Docs](https://hiver.sh/docs)

### Architecture

The Hiver runtime runs inside a container and is composed of sidecar processes. The agent sandbox runs on `runc` or `firecracker` as an untrusted workload. `sbxfuse` provides FUSE-backed volumes, `sbxproxy` transparently intercepts all TCP traffic (including TLS), and `sandboxd` wires everything together, serving the client API, reconciling sidecar policy, and streaming telemetry events.

<p align="center">
<img src="./docs/hiver-arch.svg" width="500">
</p>

The root filesystem is assembled with overlayfs, layering the agent's writes over the read-only base image for efficient snapshotting.

A typical deployment also includes a controller for sandbox lifecycle management and an Envoy gateway for external network access. All components ship out of the box but can be swapped for custom implementations.

Hiver is unopinionated about orchestration: the agent CLI or SDK can run entirely inside the sandbox or in a separate deployment. Because everything inside the sandbox is treated as untrusted, agents can call private APIs and access files without ever seeing auth tokens or secrets.

Getting started is straightforward: just run `hiver start` locally or deploy to the cloud using the same client library.

## Status

Hiver is under active development. The local runtime, inspector, audit stream, filesystem ACLs, and egress controls are usable today; APIs and deployment manifests may change before v1.

### Cold start

A core design goal of Hiver is to make a freshly started sandbox available as fast as possible. The microVM backend relies on memory snapshotting, so spawning a new VM takes milliseconds, and it exposes a hook that lets custom images do their own setup during this phase. For example, preloading a browser.

Today, scheduling onto pods goes through Kubernetes, which adds roughly a second of latency. Packing many sandboxes into a single pod — each resumed from a shared per-image base snapshot — sidesteps per-sandbox pod scheduling. In the near future, Hiver will support additional container orchestrators and drive end-to-end cold start even lower.

## License

Apache 2.0
