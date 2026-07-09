# Claude Code — TypeScript

Drive the [Claude Code](https://claude.com/claude-code) CLI inside a Hiver sandbox from a TypeScript client. The prebuilt `claude` image ships the CLI ready to go; this script provisions it and runs a one-shot task with `exec`.

Unlike the SDK examples, there's nothing to build — you run this driver **on your machine** and it talks to the sandbox over the gateway.

## Files

- `index.ts` — provisions the `claude` image and runs `claude -p` via `sandbox.exec`.
- `package.json` — depends on `@hiver.sh/client`; `start` runs the script with `tsx`.

## Run

1. Start the gateway (once):

   ```bash
   hiver up
   ```

2. Export your Anthropic API key. It's injected into the sandbox at creation and inherited by every later `exec`, so you set it once:

   ```bash
   export ANTHROPIC_API_KEY=sk-ant-...
   ```

3. Install deps and run:

   ```bash
   npm install
   npm start
   ```

The script prints Claude Code's output. Edit the prompt in `index.ts` to change the task.

> Prefer to keep the key out of the agent's reach? Provision without `env` and inject it with an egress [override](https://hiver.sh/docs/network/overrides) instead.

See the [Agent CLI docs](https://hiver.sh/docs/examples/agent-cli) for streaming output, the interactive TUI, and Claude subscription auth.
