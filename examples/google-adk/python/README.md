# Google Agent Development Kit (ADK) — Python

Run the [Google ADK](https://google.github.io/adk-docs/) loop **inside** a Hiver sandbox as an HTTP service, and drive it from a local client. ADK is Python-first (and Java), so this example is Python only. Two parts:

- **`agent/`** — the server that runs inside the sandbox. A Flask app running an ADK `Agent` behind an `InMemoryRunner` with a `bash` tool that runs commands in the sandbox's own `/workspace`. Bundled into an image with `hiver bundle`.
  - `main.py` — the server.
  - `requirements.txt` — server dependencies (`google-adk`, `flask[async]`).
  - `Dockerfile` — Python 3.13 image that runs the server on port 3000.
  - `.hiver.json` — sandbox config: a stable `image` tag, a **placeholder** `env` key (so the SDK's local auth check passes inside the sandbox), and an egress policy allowing only `generativelanguage.googleapis.com`.
- **`client.py`** — a local driver that reads your `GOOGLE_API_KEY` from the environment, injects it into the egress `override` (so it's applied at the proxy and never lives in the sandbox), provisions the sandbox from the built image, and POSTs a prompt to `proxy_url(3000)/chat`.

## Run

1. **Build the image:**

   ```bash
   hiver bundle ./agent
   ```

   Bundles `agent/` into the `google-adk-py` image (the tag comes from `.hiver.json`).

2. **Start the client** with your API key in the environment:

   ```bash
   pip install -r requirements.txt
   export GOOGLE_API_KEY=AIza...
   python client.py
   ```

   `client.py` injects the key into the egress override, provisions the sandbox, and prints the agent's reply. Running without `GOOGLE_API_KEY` set exits with an error. Edit the prompt in `client.py` to change the task.

Stop the sandbox with `hiver stop google-adk-py`.
