# Claude Agent SDK — Python

Run the [Claude Agent SDK](https://pypi.org/project/claude-agent-sdk/) loop **inside** a Hiver sandbox as an HTTP service, and drive it from a local client. Two parts:

- **`agent/`** — the server that runs inside the sandbox. A Flask app wrapping `query()`, whose built-in tools (`Bash`, `Read`, `Write`, `Edit`, `Glob`, `Grep`, `WebSearch`) resolve against the sandbox's own `/workspace`. Bundled into an image with `hiver bundle`.
  - `main.py` — the server.
  - `requirements.txt` — server dependencies (`claude-agent-sdk`, `flask[async]`).
  - `Dockerfile` — Python 3.13 image that runs the server on port 3000.
  - `.hiver.json` — sandbox config: a stable `image` tag plus an egress policy that allows only `api.anthropic.com` and injects your API key via an `override`, so the key never lives in the sandbox.
- **`client.py`** — a local driver that reads `agent/.hiver.json`, provisions the sandbox from the built image with that config, and POSTs a prompt to `proxy_url(3000)/chat`.

## Run

First, add your Anthropic API key to `agent/.hiver.json` (replace `sk-ant-...`). The egress override applies it at the proxy, so it never lives in the sandbox.

1. **Build the image:**

   ```bash
   hiver bundle ./agent
   ```

   Bundles `agent/` into the `claude-agent-sdk-py` image (the tag comes from `.hiver.json`).

2. **Start the client:**

   ```bash
   pip install -r requirements.txt
   python client.py
   ```

   `client.py` provisions the sandbox from that image and prints the agent's reply. Edit the prompt in `client.py` to change the task.

3. **(Optional) build and launch in one command:**

   ```bash
   hiver run ./agent claude-agent-sdk-py
   ```

   An alternative to step 1 that bundles `agent/` **and** launches the sandbox (reading the same `.hiver.json`). `python client.py` then attaches to the running sandbox instead of provisioning it.

Stop the sandbox with `hiver stop claude-agent-sdk-py`.

See the [Claude Agent SDK example docs](https://hiver.sh/docs/examples/agent-sdk-anthropic) for the full walkthrough.
