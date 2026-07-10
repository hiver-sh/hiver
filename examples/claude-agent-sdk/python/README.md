# Claude Agent SDK — Python

Run the [Claude Agent SDK](https://pypi.org/project/claude-agent-sdk/) loop **inside** a Hiver sandbox as an HTTP service, and drive it from a local client. Two parts:

- **`agent/`** — the server that runs inside the sandbox. A Flask app wrapping `query()`, whose built-in tools (`Bash`, `Read`, `Write`, `Edit`, `Glob`, `Grep`, `WebSearch`) resolve against the sandbox's own `/workspace`. Bundled into an image with `hiver bundle`.
  - `main.py` — the server.
  - `requirements.txt` — server dependencies (`claude-agent-sdk`, `flask[async]`).
  - `Dockerfile` — Python 3.13 image that runs the server on port 3000.
  - `.hiver.json` — sandbox config: a stable `image` tag, a **placeholder** `env` key (so the SDK's local auth check passes inside the sandbox), and an egress policy allowing only `api.anthropic.com`.
- **`client.py`** — a local driver that reads your `ANTHROPIC_API_KEY` from the environment, injects it into the egress `override` (so it's applied at the proxy and never lives in the sandbox), provisions the sandbox from the built image, and POSTs a prompt to `proxy_url(3000)/chat`.

## Run

1. **Build the image:**

   ```bash
   hiver bundle ./agent
   ```

   Bundles `agent/` into the `claude-agent-sdk-py` image (the tag comes from `.hiver.json`).

2. **Start the client** with your API key in the environment:

   ```bash
   pip3 install -r requirements.txt
   export ANTHROPIC_API_KEY=sk-ant-...
   python3 client.py
   ```

   `client.py` injects the key into the egress override, provisions the sandbox, and prints the agent's reply. Running without `ANTHROPIC_API_KEY` set exits with an error. Edit the prompt in `client.py` to change the task.

Stop the sandbox with `hiver stop claude-agent-sdk-py`.

See the [Claude Agent SDK example docs](https://hiver.sh/docs/examples/agent-sdk-anthropic) for the full walkthrough.
