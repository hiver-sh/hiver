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

## 📚 Documentation

- [TypeScript](client/typescript/README.md)
- Python (WIP)

### Supported File Systems
* Google Drive
* Google Cloud Storage
* Local
* Microsoft OneDrive (WIP)
* Azure Blob Storage (WIP)
* AWS S3 (WIP)
* Your own K/V store (WIP)
