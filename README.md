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

## 🚀 Getting Started

Install the CLI for the agent runtime:

```sh
npm install --global @hiver.sh/cli

# Or just use:
npx -y @hiver.sh/cli

# If you don't have NPM:
curl -fsSL https://hiver.sh/install.sh | sh
```

Use the CLI to manage sandboxes, stream live events, and launch the inspector — against a local stack or a remote deployment:

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

### Launch first agent

#### TypeScript

Add dependency:
```sh
npm install --save @hiver.sh/client
```

First agent:
```ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1");
const result = await sandbox.exec("claude -p 'Write a poem and save it as pdf'");
console.log(result.stdout);
```

#### Python

Add dependency:
```sh
pip install hiver-py
```

First agent:
```python
import asyncio
import hiver

async def main():
    sandbox = await hiver.get_or_create_sandbox("agent-1")
    result = await sandbox.exec("claude -p 'Write a poem and save it as pdf'")
    print(result["stdout"])

asyncio.run(main())
```

#### Go

Add dependency:
```sh
go get github.com/hiver-sh/hiver/client
```

First agent:
```go
import "github.com/hiver-sh/hiver/client"

c := client.NewClient("http://localhost:10000")
sandbox, _ := c.GetOrCreateSandbox(context.Background(), "agent-1", client.SandboxConfig{})

result, _ := sandbox.Exec(context.Background(),
    client.ExecRequest{Command: "claude -p 'Write a poem and save it as pdf'"})
fmt.Println(result.Stdout)
```

## Inspector

The inspector is the fastest way to understand what your agent is *actually* doing. Launch it with a single command:

```sh
hiver inspect agent-1
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

## Events & Loop Engineering

Every sandbox emits a structured, ordered, replayable stream of audit events: `egress.request`, `fs.request`, `exec.request`, `resource.usage`, and more, each tagged with whether the action was **allowed** or **denied**. Stream them live from the CLI:

```sh
hiver events agent-1
```

The stream is the foundation for *loop engineering*: closing the loop between what an agent tried to do and what your harness does next. Instead of letting an agent fail silently when it hits a wall, your harness can observe the denial, repair the environment or the prompt, and re-drive the agent.

A harness that grants access on demand when the agent is blocked by policy:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1");

const ac = new AbortController();

// Drive the agent and watch its behavior at the same time.
const agent = sandbox
  .exec("claude -p 'Fetch the changelog from internal-api.corp and summarize it'")
  .then((result) => { ac.abort(); return result; });

for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  if (event.type === "egress.request" && event.access === "denied") {
    // The agent tried to reach a host that policy blocks.
    // Repair the environment instead of letting the run fail.
    console.log(`unblocking ${event.host}`);
    await sandbox.applyConfig({
      egress: [{ access: "allow", host: event.host }],
    });
  }
}

console.log((await agent).stdout);
```

The same pattern powers richer loops: feed denied events back into the agent's next prompt, gate risky writes behind human approval, enforce a resource budget from `resource.usage`, or record the full stream and replay it deterministically for evals. Because events are ordered and carry an `id`, your harness can resume exactly where it left off after a disconnect.

## File Systems

A sandbox's filesystem is assembled from mounts you declare, each backed by local storage, Google Drive, Google Cloud Storage, OneDrive, S3, or Azure Blob. Every mount is FUSE-backed by `sbxfuse`, so the agent sees ordinary files and directories, but every read and write passes through the runtime, where it's checked against **path-level ACLs** and emitted as an auditable `fs.request` event.

This is what lets you safely put real data in front of an agent. Mount **organization knowledge** like skills, runbooks, docs, and design specs **read-only**, so the agent can ground its work in it but can never corrupt the source of truth. Mount the user's **personal data** read-write in a scratch area for the agent to produce output. Lock everything else down with `deny`. Each ACL rule is just a `path` and an access level of `ro`, `rw`, or `deny`, evaluated most-specific-first, so you can open a tree and carve exceptions out of it:

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

Because the ACL is enforced by the runtime rather than the agent, a misbehaving or jailbroken agent still can't write to a read-only mount or read a denied path, and every attempt, allowed or denied, shows up in the event stream and the inspector's **Files** view.

### Network File System

Point a mount at any HTTP host that implements the [file system interface](api/external_file_system.yaml) and the runtime turns every read and write into a call against it — bring your own storage without a built-in backend:

```typescript
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1", {
  fs: [{ backend: "external", mount: "/data", host: "https://fs.internal:8080" }],
});
```

## Request Overrides

The egress proxy doesn't just allow or deny requests. It can **rewrite** them on the way out. Attach an `override` to an egress rule and the proxy can inject **headers** and **query parameters**, redirect the request to a different **upstream host**, and prepend a **path prefix** — all on every matching request, overwriting whatever the agent set. This is how you give an agent authenticated access to an API while the agent itself never holds the credential:

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

The agent makes a plain request to `api.internal.acme.com`; the proxy adds the `Authorization` header and the `tenant` / `api-version` query params before the request leaves the sandbox. Because the secret is bound to the rule and applied outside the untrusted workload, a prompt-injected or compromised agent can spend the token against the allowed host but can never read, exfiltrate, or reuse it elsewhere. Combined with a default-deny egress policy, this lets you hand an agent exactly one authenticated integration and nothing more, and every rewritten request still shows up in the inspector's **Network** view.

### Redirecting to a mock server

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

The agent issues a normal `https://api.openai.com/v1/models` request; the proxy dials `mockserver.internal:8080` and forwards it as `/openai/v1/models`, while the agent still sees `api.openai.com` as the host. This is ideal for deterministic tests, replaying recorded fixtures, or injecting faults without the agent ever knowing the traffic was diverted.

### Gating a request behind user consent

Start from a default-deny policy and treat each blocked host as a prompt for human approval. The event stream surfaces the `egress.request` denial; your harness pauses, asks the user, and only then opens the host — turning policy denials into an interactive allow-list the user builds up as the agent runs:

```typescript
import * as hiver from "@hiver.sh/client";
import { confirm } from "./prompt"; // your UI: returns a Promise<boolean>

const sandbox = await hiver.getOrCreateSandbox("agent-1");
const approved = new Set<string>();
const ac = new AbortController();

const agent = sandbox
  .exec("claude -p 'Research the topic and post a summary to our CRM'")
  .then((result) => { ac.abort(); return result; });

for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  if (event.type === "egress.request" && event.access === "denied") {
    if (approved.has(event.host)) continue;
    approved.add(event.host);

    // Ask the user before letting the agent reach a new host.
    const ok = await confirm(`Agent wants to reach ${event.host}. Allow?`);
    if (!ok) continue; // leave the host blocked; the agent sees the denial

    await sandbox.applyConfig({
      egress: [{ access: "allow", host: event.host }],
    });
  }
}

console.log((await agent).stdout);
```

The first request to each host is denied and the agent stalls there; the user decides, and an approval flips the rule to `allow` so the agent's retry succeeds.


## Documentation

* [Docs](https://hiver.sh/docs)

* [Examples](https://hiver.sh/docs/examples)

### Isolation Modes
Container-level isolation using [`runc`](https://github.com/opencontainers/runc) and kernel-level isolation using [`firecracker`](https://github.com/firecracker-microvm/firecracker).

### File Systems
Local, External over HTTP, Google Drive, Google Cloud Storage, Microsoft OneDrive, Amazon S3,Azure Blob Storage.

### Container Orchestration
Docker, k8s.


### Architecture

The Hiver runtime runs inside a container and is composed of sidecar processes. The agent sandbox runs on `runc` or `firecracker` as an untrusted workload. `sbxfuse` provides FUSE-backed volumes, `sbxproxy` transparently intercepts all TCP traffic (including TLS), and `sandboxd` wires everything together — serving the client API, reconciling sidecar policy, and streaming telemetry events.

<p align="center">
<img src="./docs/hiver-arch.svg" width="500">
</p>

The root filesystem is assembled with overlayfs, layering the agent's writes over the read-only base image for efficient snapshotting.

A typical deployment also includes a controller for sandbox lifecycle management and an Envoy gateway for external network access. All components ship out of the box but can be swapped for custom implementations.

Hiver is unopinionated about orchestration: the agent CLI or SDK can run entirely inside the sandbox or in a separate deployment. Because everything inside the sandbox is treated as untrusted, agents can call private APIs and access files without ever seeing auth tokens or secrets.

Getting started is straightforward — just run `hiver start` locally or deploy to the cloud using the same client library.

## License

Apache 2.0
