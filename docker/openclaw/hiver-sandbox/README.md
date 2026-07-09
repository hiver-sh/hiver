# openclaw-hiver-sandbox

An OpenClaw plugin that registers a `hiver` **sandbox backend**. When the gateway
is configured with `agents.defaults.sandbox.backend = "hiver"`, OpenClaw runs
sandboxed tool calls inside a [Hiver](https://hiver.sh) sandbox instead of a
local Docker container:

- **exec / process** — the backend's `buildExecSpec` returns an argv that spawns
  [`src/exec-shim.mjs`](./src/exec-shim.mjs), a thin child process that streams
  the command through `sandbox.execStream` (stdin, stdout/stderr, exit code, and
  optional PTY are all forwarded).
- **read / write / edit / apply_patch** — the backend's fs bridge fulfils
  OpenClaw's `SandboxFsBridge` using Hiver's native file API for binary-safe
  reads/writes plus shell commands for `mkdirp`/`rename`/`stat`/`remove`. Files
  land in the Hiver sandbox, not on the gateway host.

The gateway process itself keeps running on the host; only tool execution
crosses into the sandbox.

## Configuration

The backend spawns one Hiver sandbox per OpenClaw sandbox scope (see
`agents.defaults.sandbox.scope`), keyed by a hash of the scope key.

| Env var             | Purpose                                                        | Default                         |
| ------------------- | ------------------------------------------------------------- | ------------------------------- |
| `HIVER_GATEWAY_URL` | Hiver control-plane URL the backend provisions sandboxes from | `@hiver.sh/client` default      |
| `HIVER_SANDBOX_IMAGE` | Logical Hiver image the nested sandbox boots                | client default (`agent-base`)   |

## Install

```bash
npm install --omit=dev
openclaw plugins install .
openclaw config set agents.defaults.sandbox.mode all
openclaw config set agents.defaults.sandbox.backend hiver
```

This is baked into `docker/openclaw/Dockerfile`.
