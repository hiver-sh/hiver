# OpenAI Agents SDK — TypeScript

Run the [OpenAI Agents SDK](https://openai.github.io/openai-agents-js/) loop **inside** a Hiver sandbox as an HTTP service, and drive it from a local client. Two parts:

- **`agent/`** — the server that runs inside the sandbox. An Express app running the agent with `bash`/`read_file`/`write_file` function tools that resolve against the sandbox's own `/workspace` (the SDK ships no local file tools). Bundled into an image with `hiver bundle`.
  - `index.ts` — the server.
  - `package.json` — server dependencies.
  - `Dockerfile` — Node 22 image that runs the server on port 3000.
  - `.hiver.json` — sandbox config: a stable `image` tag, a **placeholder** `env` key (so the SDK's local auth check passes inside the sandbox), and an egress policy allowing only `api.openai.com`.
- **`client.ts`** — a local driver that reads your `OPENAI_API_KEY` from the environment, injects it into the egress `override` (so it's applied at the proxy and never lives in the sandbox), provisions the sandbox from the built image, and POSTs a prompt to `proxyUrl(3000)/chat`.

## Run

1. **Build the image:**

   ```bash
   npm run build
   ```

   Bundles `agent/` into the `openai-agents-sdk-ts` image (`hiver bundle ./agent`; the tag comes from `.hiver.json`).

2. **Start the client** with your API key in the environment:

   ```bash
   npm install
   export OPENAI_API_KEY=sk-...
   npm start
   ```

   `client.ts` injects the key into the egress override, provisions the sandbox, and prints the agent's reply. Running without `OPENAI_API_KEY` set exits with an error. Edit the prompt in `client.ts` to change the task.

Stop the sandbox with `hiver stop openai-agents-sdk-ts`.

See the [OpenAI Agents SDK example docs](https://hiver.sh/docs/examples/agent-sdk-openai) for the full walkthrough.
