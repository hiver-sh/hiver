<p align="center">
<img src="./docs/hive.svg" width="100" align="center">
</p>
<h1 align="center">Hive Sandbox</h1>

Hive gives an agent a stateful sandbox with its own file system _(Google Drive, OneDrive, S3, GCS, Azure Blob Storage, or local)_ and network access — with every read, write, and outbound request mediated and logged. The agent runs unmodified: it executes standard bash commands, and Hive handles the rest.

Give the agent APIs and it will write code, analyze data, and produce reports — accumulating skills, findings, and artifacts over time. Public data sources and private or user-specific ones can be combined freely. Work is never lost: the persistent file system doubles as a checkpoint store.

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

<img src="./docs/hive-diagram.svg" width="500">

A Hive sandbox is composed of an orchestrator (sbxd), 2 sidecar processes, and an agent container run via runc.

### Components

* sbxd: The orchestrator. Provides the API server and manages the lifecycle of the sidecars and agent container.
* sbxfuse: A FUSE filesystem sidecar that mediates and logs all file access. One instance runs per configured mount.
* sbxproxy: A transparent TCP proxy sidecar that enforces egress ACLs and logs all outbound requests.
* Agent container: Runs in an isolated OCI container via [runc](https://github.com/opencontainers/runc). By default the image runs an MCP server that provides tools like `Bash` and `Read` to agents.


### Supported File Systems
* Google Drive
* Google Cloud Storage
* Local
* Microsoft OneDrive (WIP)
* Azure Blob Storage (WIP)
* AWS S3 (WIP)
* Your own K/V store (WIP)
