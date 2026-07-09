# Browser Use — Python

Drive the resident headless Chromium in the prebuilt `browser` image with [Playwright](https://playwright.dev/python/) over CDP. All the automation logic lives in this client script; the browser runs isolated in the sandbox, reached through the [proxy](https://hiver.sh/docs/ingress#exposed-ports) at a stable `/cdp` path on port **9223**.

Unlike the SDK examples, there's nothing to build — you run this driver **on your machine** and it connects to the sandbox's browser over the gateway. Your local Playwright version is independent of the Chromium baked into the image, because CDP is a wire protocol.

## Files

- `main.py` — provisions the `browser` image, connects Playwright over CDP, and scrapes a page.
- `requirements.txt` — the `hiver` client and `playwright`.

## Run

1. Start the gateway (once):

   ```bash
   hiver up
   ```

2. Install deps and run (no API key needed — you're driving the browser directly):

   ```bash
   pip install -r requirements.txt
   python main.py
   ```

The script prints the top Hacker News titles. You don't need `playwright install`: `connect_over_cdp` attaches to the sandbox's Chromium instead of launching a local one.

> `browser.close()` only disconnects your CDP client — the resident browser stays warm for the next attach.

To let an **agent** drive the browser instead (via the `browser` skill), see the [Claude Code example](../../claude-code/) and the [Browser Use docs](https://hiver.sh/docs/examples/browser-use).
