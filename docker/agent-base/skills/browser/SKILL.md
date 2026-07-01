---
name: browser
description: Drive a real headless Chrome in a sandbox via the Chrome DevTools Protocol (CDP). Use when you need to load JavaScript-heavy or login-gated pages, click/type/scroll, fill forms, or extract content that a plain HTTP fetch can't render. Prefer reading the accessibility tree to understand a page; screenshots are a last resort. Not for simple static-page fetches.
---

# Browser

Start the bridge by running `cdp-bridge.js`:

```bash
node /home/agent/.claude/skills/browser/scripts/cdp-bridge.js > /tmp/cdp-bridge.log 2>&1 &
echo $! > /tmp/cdp-bridge.pid
# wait ~3s for it to connect, then check:
cat /tmp/cdp-bridge.log
```

The bridge is ready when the log shows `ready on /tmp/cdp.sock`.

## Sending CDP commands

Connect to `/tmp/cdp.sock` and write newline-terminated JSON. Responses are broadcast back on the same connection.

**Using `cdp-send.js`:**

```bash
node /home/agent/.claude/skills/browser/scripts/cdp-send.js '{"id":1,"method":"Browser.getVersion","params":{}}'
```

## Reading the page

To understand page content, **prefer the accessibility tree** — it's a compact, structured, text representation of what's on screen (roles, names, values) and is far cheaper and more reliable to reason over than an image.

```bash
# enable, then fetch the full accessibility tree
node /home/agent/.claude/skills/browser/scripts/cdp-send.js '{"id":1,"method":"Accessibility.enable","params":{}}'
node /home/agent/.claude/skills/browser/scripts/cdp-send.js '{"id":2,"method":"Accessibility.getFullAXTree","params":{}}'
```

You can also read the DOM/text directly (e.g. `DOM.getDocument`, `Runtime.evaluate` returning `document.body.innerText`) when the accessibility tree isn't enough.

**Screenshots are a last resort** — only capture one (`Page.captureScreenshot`) when the accessibility tree and DOM don't give you what you need, such as verifying visual layout, rendering, or content that isn't exposed as text.

## Stopping the bridge

```bash
kill $(cat /tmp/cdp-bridge.pid)
```
