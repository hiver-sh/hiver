# Examples

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
