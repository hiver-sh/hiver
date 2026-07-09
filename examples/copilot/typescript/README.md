# GitHub Copilot CLI — TypeScript

Drive the [GitHub Copilot CLI](https://github.com/github/copilot-cli) inside a Hiver sandbox from a TypeScript client. The prebuilt `copilot` image ships the CLI; this script provisions it and runs a one-shot task with `exec`.

Unlike the SDK examples, there's nothing to build — you run this driver **on your machine** and it talks to the sandbox over the gateway.

## Files

- `index.ts` — provisions the `copilot` image and runs `copilot -p` via `sandbox.exec`.
- `package.json` — depends on `@hiver.sh/client`; `start` runs the script with `tsx`.

## Run

1. Start the gateway (once):

   ```bash
   hiver up
   ```

2. Export your GitHub token. It's injected into the sandbox at creation and inherited by every later `exec`, so you set it once:

   ```bash
   export GITHUB_TOKEN=ghp_...
   ```

3. Install deps and run:

   ```bash
   npm install
   npm start
   ```

The script prints Copilot's output. Edit the prompt in `index.ts` to change the task.

> Prefer to keep the token out of the agent's reach? Provision without `env` and inject it with an egress [override](https://hiver.sh/docs/network/overrides) instead.

See the [Agent CLI docs](https://hiver.sh/docs/examples/agent-cli) for streaming output and the interactive TUI.
