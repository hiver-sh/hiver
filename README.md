<p align="center">
<img src="./docs/hive.svg" width="100">
</p>
<h1 align="center">Hive Sandbox</h1>

Hive gives an agent a stateful sandbox with its own file system _(Google Drive, OneDrive, S3, GCS, Azure Blob Storage, or local)_ and network access. Every read, write, and outbound request mediated and logged. The agent runs unmodified: it executes standard bash commands, and Hive handles the rest.

## 🚀 Getting Started

Start the controller locally:
```sh
make up
```

Example client:
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
```

Find a complete example [Stateful claude agent](client/typescript/examples/README.md).

## Documentation

- [TypeScript](client/typescript/README.md)
- Python (WIP)

## How it works

<p align="center">
<img src="./docs/hive-diagram.svg" width="500">
</p>

A Hive sandbox is composed of an orchestrator (sbxd), 2 sidecar processes, and an agent container run via runc.

### Components

* **sbxd**: The orchestrator. Provides the API server and manages the lifecycle of the sidecars and agent container.
* **sbxfuse**: A FUSE filesystem sidecar that mediates and logs all file access. One instance runs per configured mount.
* **sbxproxy**: A transparent TCP proxy sidecar that enforces egress ACLs and logs all outbound requests. The agent does not need to set `HTTP_PROXY` — the kernel redirects all outbound TCP to sbxproxy transparently, so no changes to agent code are required. A CA certificate is automatically generated and injected into the agent container so TLS connections are trusted without any manual certificate configuration.
* **Agent container**: Runs in an isolated OCI container via [runc](https://github.com/opencontainers/runc). By default the image runs an MCP server that provides tools like `Bash` and `Read` to agents.
* **Controller**: Creates and destroys a sandbox.

Beyond security and ease of use, this architecture lets agents share a persistent file system. Each agent can accumulate its own findings, and a coordinating agent can read across all of them to synthesize global insights. This applies to skills, reports, or any other artifacts worth sharing.

<p align="center">
<img src="./docs/multi-agent.svg" width="300">
</p>

### Supported File Systems
* Google Drive
* Google Cloud Storage
* Local
* Microsoft OneDrive (WIP)
* Azure Blob Storage (WIP)
* Amazon S3 (WIP)
* Your own K/V store (WIP)

## Why

Give the agent APIs and it will write code, analyze data, and produce reports — accumulating skills, findings, and artifacts over time. Public data sources and private or user-specific ones can be combined freely. Work is never lost: the persistent file system doubles as a checkpoint store. Hive not only saves on repeative work and tokens, but also increases the agent intelligence while providing enterprise-level security.

## License

Apache 2.0
