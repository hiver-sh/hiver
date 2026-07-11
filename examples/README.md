# Examples

## ⭐ Start here: full agent with browser use

**[`Open Work`](./open-work/)** is the best place to start if you want to see everything working together: a complete Next.js app that runs a full agent inside the sandbox with browser use, driving a real Chromium and streaming the work back to a live chat UI. Start a task, attach files or a local folder, `@`-reference folder files, and watch file edits.

```sh
cd next.js && npm install && npm run dev   # http://localhost:3000
```

---

Runnable agent examples for Hiver, in TypeScript and Python. They come in two shapes:

- **Agent SDK servers** — a self-contained project (agent server + `Dockerfile` + `.hiver.json`) that runs the agent loop **inside** the sandbox as an HTTP service. You launch it with `hiver run . <key>`, which bundles the directory into an image and starts it. The `.hiver.json` locks egress to the model provider and injects the API key via an `override`, so the key is applied by the proxy after the request leaves the sandbox and never lives in the sandbox's env or context.
- **CLI / browser drivers** — a client script you run **on your machine**. It provisions a prebuilt image (`claude`, `codex`, `copilot`, `browser`) and drives it over the gateway with `exec` or CDP. Start the gateway with `hiver up` first.

See each example's `README.md` for the exact command, the key/image to use, and how to send it a prompt.

## Agent SDK servers (`hiver run .`)

| Example | TypeScript | Python |
| --- | --- | --- |
| [Claude Agent SDK](claude-agent-sdk/) | [`typescript/`](claude-agent-sdk/typescript/) | [`python/`](claude-agent-sdk/python/) |
| [OpenAI Agents SDK](openai-agents-sdk/) | [`typescript/`](openai-agents-sdk/typescript/) | [`python/`](openai-agents-sdk/python/) |
| [Google ADK](google-adk/) | — | [`python/`](google-adk/python/) |

Google ADK is Python-first (and Java), so it has no TypeScript example.

## CLI / browser drivers (`hiver up`, then run locally)

| Example | TypeScript | Python |
| --- | --- | --- |
| [Claude Code](claude-code/) | [`typescript/`](claude-code/typescript/) | [`python/`](claude-code/python/) |
| [Codex](codex/) | [`typescript/`](codex/typescript/) | [`python/`](codex/python/) |
| [GitHub Copilot CLI](copilot/) | [`typescript/`](copilot/typescript/) | [`python/`](copilot/python/) |
| [Browser Use](browser-use/) | [`typescript/`](browser-use/typescript/) | [`python/`](browser-use/python/) |

## Client SDK examples (`hiver up`, then run locally)

Lower-level examples that drive the sandbox directly with the [`@hiver.sh/client`](../client/typescript/) SDK — `exec`, config, egress, snapshots, mounts, proxied services, and agents. TypeScript only. Each is standalone (`npm install` in its `typescript/` directory, then `npm start`).

| Example | What it shows |
| --- | --- |
| [`apply-config`](apply-config/typescript/) | Read the config, add an egress rule, apply the update. |
| [`egress-events`](egress-events/typescript/) | Observe `egress.request` / `egress.response` events for allowed vs blocked hosts. |
| [`node-internal-service`](node-internal-service/typescript/) | Start deny-all, then `applyConfig` to allow an internal host. |
| [`node-exec`](node-exec/typescript/) / [`python-exec`](python-exec/typescript/) | Run a command and read the buffered result. |
| [`node-exec-stream`](node-exec-stream/typescript/) / [`python-exec-stream`](python-exec-stream/typescript/) | Stream command output over SSE. |
| [`node-exec-tty`](node-exec-tty/typescript/) / [`python-exec-tty`](python-exec-tty/typescript/) | Drive an interactive REPL over a TTY exec stream. |
| [`claude-exec`](claude-exec/typescript/) | Run Claude inside the `claude` image and print the result. |
| [`files`](files/typescript/) | Upload a file into a mount and read it back out. |
| [`list-directory`](list-directory/typescript/) | List the contents of a sandbox mount. |
| [`terminal-attach`](terminal-attach/typescript/) | Attach your local terminal to an interactive shell in the sandbox. |
| [`snapshot`](snapshot/typescript/) | Persist a sandbox's filesystem across a full shutdown via snapshots. |
| [`snapshot-fuse`](snapshot-fuse/typescript/) | Route the snapshot through an internal GCS-backed FUSE drive. |
| [`local-filesystem-mount`](local-filesystem-mount/typescript/) | Mount a local directory into the sandbox during development. |
| [`gdrive-filesystem`](gdrive-filesystem/typescript/) | Mount a Google Drive folder over a FUSE mount. |
| [`http-server`](http-server/typescript/) | Proxy HTTP requests to a service running inside the sandbox. |
| [`mcp-server`](mcp-server/typescript/) | Bundle and run an MCP server inside the sandbox. |
| [`browser-cdp`](browser-cdp/typescript/) | Drive the sandbox's resident Chromium with Playwright over CDP. |
| [`claude-agent`](claude-agent/typescript/) | A Claude agent that drives the sandbox through an in-sandbox MCP server. |
| [`claude-agent-gdrive-filesystem`](claude-agent-gdrive-filesystem/typescript/) | The Claude agent, with generated files persisted to Google Drive. |
