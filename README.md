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

```sh
curl -fsSL https://hiver.sh/install.sh | sh

hiver up
```

### Launch first agent

#### TypeScript

```ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1");
const result = await sandbox.exec("claude -p 'Write a poem and save it as pdf'");
console.log(result.stdout);
```

#### Python

```py
import hiver

sandbox = hiver.get_or_create_sandbox("agent-1")

result = sandbox.exec("claude -p 'Write a poem and save it as pdf'")
print(result.stdout)
```

#### Go

```go
import "github.com/hive-run/hive-runtime/client"

sandbox, _ := hive.GetOrCreateSandbox("agent-1", hive.SandboxConfig{})

result, _ := sandbox.Exec("claude -p 'Write a poem and save it as pdf'")
fmt.Println(result.Stdout)
```

### Hiver CLI

#### Start stack

`hiver up`

#### Stop stack

`hiver down`

#### Bundle a custom Docker image

`hiver bundle ./custom-agent custom-agent:latest`

#### Launch DevTools

`hiver devtools`

#### List sandboxes

`hiver list`

#### Sandbox events

`hiver events --sandbox agent-1 --follow`

## Documentation

* [Docs](https://hiver.sh/docs)

* [Self-improving loop](https://hiver.sh/docs/self-improving)

* [Examples](https://hiver.sh/docs/examples)

### Isolation Modes
Container-level isolation using [`runc`](https://github.com/opencontainers/runc) and kernel-level isolation using [`firecracker`](https://github.com/firecracker-microvm/firecracker).

### File Systems
Local, Google Drive, Google Cloud Storage, Microsoft OneDrive, Amazon S3,Azure Blob Storage.

### Container Orchestration
Docker, k8s.

## Why

Give an agent APIs and a persistent workspace, and it can write code, analyze data, generate reports, and accumulate artifacts across runs. Public and private data sources can be combined seamlessly within the same environment.

The filesystem doubles as durable state and a checkpoint store, allowing agents to resume work without rebuilding context each session. By persisting intermediate artifacts, caches, and outputs, Hive reduces redundant computation and token usage while enforcing auditable filesystem and network boundaries.

## License

Apache 2.0
