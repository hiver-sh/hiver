<p align="center">
<img src="./docs/hive.svg" width="100">
</p>
<h1 align="center">Hiver</h1>
<h3 align="center">Chrome DevTools for Agents</h3>

<p align="center">
Replay every browser action, file change, network request, tool call, and approval.
</p>

<p align="center">
<img src="./docs/devtools.png">
</p>

## What is Hiver?

Hiver is the platform for running AI agents as untrusted workloads with visibility and control over their file, network, command, tool and model interactions. It has two parts:

- **The runtime** boots each agent into an isolated sandbox in milliseconds, with its own file systems, network policy, and path-level ACLs. Use MicroVM isolation for fully untrusted code that needs its own kernel or containers for local development behind the same API. Every command, file access, and network request is mediated by the runtime and emitted as a structured, replayable audit event.
- **The inspector** is a live, DevTools-style UI over running sandboxes. It decodes the agent’s LLM traffic into readable conversations, shows every egress request and file operation with its allowed/denied verdict, surfaces a timeline of activity, and lets you edit sandbox policy on the fly — all over the same event stream and API used by the SDKs.

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
⬢ Hiver · Agent Runtime v0.1.26

  Usage: hiver <command> [options]

  Commands
    up       Bring up local stack
    down     Bring down local stack
    connect  Connect to remote stack
    start    Start a sandbox
    run      Build and launch a project directory as a sandbox
    stop     Stop a sandbox
    shell    Open an interactive shell in a sandbox
    list     List the sandboxes
    events   Stream a sandbox's events live as they happen
    inspect  Launch the inspector
    bundle   Bundle a Docker image into a Hiver runtime image

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

Find [`examples`](examples/README.md) in TypeScript, Python and Go.

**⭐ [`examples/open-work`](examples/open-work/)** — Open Work, a full Next.js app demonstrating a complete agent with security, elicitation, content collaboration with AI and a Web browser.

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

See the [Claude Agent SDK example](examples/claude-agent-sdk/) for a complete, runnable version.

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
hiver events claude-code-an \
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

## Cloud Deployment

Run the full stack (controller, gateway, and sandboxes) on a managed Kubernetes cluster, and drive it from the same client library you use locally.

Deployment is split into two pieces: provisioning a cluster, and installing the control plane onto it.

### Google Kubernetes Engine (GKE)

[`deployment/gke`](deployment/gke/README.md) provisions a GKE cluster with Terraform. The node pool is configured for microVM isolation out of the box: nested virtualization is enabled so KVM works inside the nodes, and nodes are backed by Local SSD for fast snapshot and prewarm I/O. See the [GKE deployment guide](deployment/gke/README.md) for prerequisites and `terraform apply` steps.

### Helm Chart

[`deployment/k8s/chart`](deployment/k8s/chart/README.md) is a Helm chart that deploys the controller, the Envoy gateway, and the per-image sandbox pools onto any Kubernetes cluster. The gateway's per-image routes and each image's Deployment+Service are generated from a single `images` list, so adding an image is one entry. Install with `helm upgrade --install hiver deployment/k8s/chart`; see the [chart README](deployment/k8s/chart/README.md) for prerequisites and configuration.


### Architecture

Hiver runs untrusted agent workloads inside `runc` or `firecracker` sandboxes, with sidecar processes (`sbxfuse`, `sbxproxy`, `sandboxd`) providing FUSE-backed volumes, transparent TCP/TLS interception, and the client API. For a full walkthrough of the runtime, isolation model, and file system design, see the [Architecture documentation](docs/architecture/page.mdx).

## Status

Hiver is under active development. The local runtime, inspector, audit stream, filesystem ACLs, and egress controls are usable today; APIs and deployment manifests may change before v1.

## License

Apache 2.0
