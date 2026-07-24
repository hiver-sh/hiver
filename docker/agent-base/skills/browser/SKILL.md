---
name: browser
description: Drive a headless browser via the Chrome DevTools Protocol (CDP). Use when you need to load JavaScript-heavy or login-gated pages, click/type/scroll, fill forms, or extract content that a plain HTTP fetch can't render. Prefer reading the accessibility tree to understand a page; screenshots are a last resort. Not for simple static-page fetches.
---

# Browser

## Quick start — open a URL in one step

Start the bridge in the background and open your target URL in a **single command**.
`cdp-open.js` retries the socket until the bridge is up, then attaches to the page,
navigates, waits for load, and prints the page's `sessionId`:

```bash
node /home/agent/.claude/skills/browser/scripts/cdp-bridge.js > /tmp/cdp-bridge.log 2>&1 &
echo $! > /tmp/cdp-bridge.pid
node /home/agent/.claude/skills/browser/scripts/cdp-open.js "https://mail.google.com/mail/u/0/#inbox"
# -> {"sessionId":"<SID>","url":"https://mail.google.com/mail/u/0/#inbox"}
```

Keep that `<SID>` — pass it to `cdp-eval.js` / `cdp-send.js` for every later
interaction (see below). Because `cdp-open.js` already did the attach, you do **not**
need a separate `Target.attachToTarget` step. To navigate again later, just call
`cdp-open.js "<url>"` again.

Do the whole opening in **one** tool call — launching the bridge and running
`cdp-open.js` back-to-back — rather than as separate steps; each extra step is an
extra model round-trip before the page even starts loading.

## Starting the bridge manually

If you need the bridge without immediately navigating (e.g. to drive an
already-open page), start it alone:

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

### Attach to the page target first (mandatory, once per session)

If you opened the page with `cdp-open.js`, it already attached and printed the
`sessionId` — reuse that and **skip this section**. Only do the manual attach below
when you started the bridge without navigating.

Before any `Runtime.evaluate` / DOM work you **must** attach to the page target and
then pass its `sessionId` on **every** subsequent command. Skipping this doesn't
error — commands run against the wrong (browser-level) scope and evals silently come
back `undefined`, which is a slow thing to debug. Do it once, right after the bridge
is ready:

```bash
# 1. list targets, find the one with "type":"page"
node .../cdp-send.js '{"id":1,"method":"Target.getTargets","params":{}}'
# 2. attach to that page target with flatten:true — the reply carries a sessionId
node .../cdp-send.js '{"id":2,"method":"Target.attachToTarget","params":{"targetId":"<pageTargetId>","flatten":true}}'
# -> {"id":2,"result":{"sessionId":"<SID>"}}
```

From here on, **include `"sessionId":"<SID>"`** at the top level of every command
(`cdp-send.js`), or pass it as the 2nd arg to `cdp-eval.js` (below).

### Running JS in the page — use `cdp-eval.js`

For any DOM action (click, type, read a value), don't hand-build a `Runtime.evaluate`
payload and pipe it through `cdp-send.js` — that's a write-file → build-JSON → send
dance of three tool calls per interaction. Run it in **one** call with `cdp-eval.js`,
which reads your JS, wraps it, and sends it.

**Prefer piping the script over stdin** (pass `-` as the file arg) — no scratch file
needed. Use a **quoted heredoc** (`<<'JS'`) so the shell passes the JS through
literally, side-stepping the embedded-text quoting problem entirely:

```bash
node /home/agent/.claude/skills/browser/scripts/cdp-eval.js - <SID> <<'JS'
const el = document.querySelector('[aria-label="Reply"]');
el.click();
return el.getAttribute('aria-label');
JS
# prints the script's returned value as JSON
```

Only write the JS to a file when you want to keep/iterate on it; then pass the path
instead of `-`: `cdp-eval.js /workspace/click.js <SID>`.

Pass the attached target's `<SID>` (from the attach step above) as the arg after the
script source — without it the script evaluates in the wrong scope and returns
`undefined`.

`cdp-eval.js` wraps the script in an **async IIFE**, so you can `await` and `return` a
value, and — critically — your declarations are scoped.

**Always wrap injected scripts in an IIFE** (`cdp-eval.js` does this for you; do it by
hand if you call `Runtime.evaluate` directly). A raw `Runtime.evaluate` runs in the
page's shared global scope that **persists across calls**, so a top-level
`const el = ...` in one script makes the next script reusing `el` throw
`Identifier 'el' has already been declared`. Wrapping in `(async () => { ... })()`
gives each script its own scope and avoids the collision.

## Reading the page

To understand page content, **prefer text over an image** — roles, names, and values are far cheaper and more reliable to reason over than a screenshot.

**Default to targeted reads.** Pull just the region you care about with `Runtime.evaluate` — e.g. `document.querySelector('...').innerText`, or a small array of `{text, ariaLabel}` for candidate elements. These are fast, precise, and cheap:

```bash
node .../cdp-send.js '{"id":1,"method":"Runtime.evaluate","params":{"expression":"document.querySelector(\"[role=main]\").innerText","returnByValue":true}}'
```

**Use `Accessibility.getFullAXTree` sparingly.** A full-tree dump is large and mostly noise for a focused task — reach for it only when you genuinely need the whole page's structure (roles/names) and targeted reads aren't enough. When you do need the tree, prefer a scoped query (`Accessibility.queryAXTree` on a node) over dumping everything.

```bash
node .../cdp-send.js '{"id":1,"method":"Accessibility.enable","params":{}}'
node .../cdp-send.js '{"id":2,"method":"Accessibility.getFullAXTree","params":{}}'
```

**Screenshots are a last resort** — see below.

### Selecting elements

Prefer **stable attribute selectors** — `[aria-label="..."]`, `[data-*]`, `role`, `id` — over matching on visible text content. Text matching is brittle: it hits the wrong node when the label appears in multiple places (Gmail, for instance, repeats "Reply" across the UI), breaks on whitespace/case, and picks up hidden or duplicate elements. Reach for text matching only when no stable attribute is available.

### Typing / setting text (encoding)

Content is UTF-8. When you inject text via `Runtime.evaluate`, **do not decode with `atob()`** — it decodes byte-per-char and mangles any non-ASCII (em dashes, curly quotes, accents) into mojibake. Either:

- pass the string directly (properly JSON-escaped) in the `expression`/`value`, or
- if you must base64 to avoid escaping, decode UTF-8-aware: `new TextDecoder().decode(Uint8Array.from(atob(b64), c => c.charCodeAt(0)))`.

When in doubt, keep injected content ASCII-only (`-` instead of `—`, `"` instead of `"`) to sidestep the issue entirely.

**Injecting JS with embedded text? Write a throwaway Node script, don't inline it.** A `Runtime.evaluate` expression that contains a text string quickly hits shell quoting hell through bash/heredoc (and Python one-liners fare no better). Skip straight to writing a small `.js` file and running it with `node` — that's the reliable first move, not the fallback.

**Screenshots are a last resort** — only capture one (`Page.captureScreenshot`) when the accessibility tree and DOM don't give you what you need, such as verifying visual layout, rendering, or content that isn't exposed as text.

## Uploading / attaching a file

Headless Chrome has no OS file picker, so clicking an "Attach"/upload button
opens a native dialog that never renders and the flow just hangs. The fix is to
set the file directly on the page's `<input type=file>` over CDP. Always stage
the file first, then **try the direct-DOM path before the chooser interception** —
most pages (Gmail included) already have a usable `<input type=file>` in the DOM,
and the direct path is fewer round trips and doesn't hang.

**Step 1 — stage the file in the sandbox.** `DOM.setFileInputFiles` takes file
*paths on the machine running Chrome*, not bytes, so the file has to exist inside
the browser-vm first. `write-file.js` uploads a local file and prints its
sandbox-visible path:

```bash
node /home/agent/.claude/skills/browser/scripts/write-file.js ./report.pdf
# -> {"status":"ok","path":"/workspace/report.pdf","bytes":12345}
```

**Step 2 — try the direct DOM path first.** Look for an existing file input and
set the staged path on it directly. This is the common case (Gmail's attach
control has one) and should be your first attempt:

```bash
node .../cdp-send.js '{"id":1,"method":"DOM.getDocument","params":{}}'
node .../cdp-send.js '{"id":2,"method":"DOM.querySelector","params":{"nodeId":<docNodeId>,"selector":"input[type=file]"}}'
# if that returns a non-zero nodeId, set files on it and you're done:
node .../cdp-send.js '{"id":3,"method":"DOM.setFileInputFiles","params":{"nodeId":<id>,"files":["/workspace/report.pdf"]}}'
```

Setting files populates `input.files` and fires the `change` event — exactly as
if a human had picked the file — so the page starts the upload. Then continue the
normal flow (e.g. click "Send"). **Only fall back to Step 3 if no
`input[type=file]` exists** (querySelector returns nodeId `0`).

**Step 3 — fallback: intercept the file chooser.** Use this only when the page
has no reachable file input and instead opens a native picker on click. Turn that
dialog into a CDP event:

```bash
node .../cdp-send.js '{"id":1,"method":"Page.enable","params":{}}'
node .../cdp-send.js '{"id":2,"method":"Page.setInterceptFileChooserDialog","params":{"enabled":true}}'
```

Click the upload button the normal way (accessibility node / `Runtime.evaluate`
click). Chrome emits a `Page.fileChooserOpened` event instead of a dialog,
carrying a `backendNodeId` for the input:

```json
{"method":"Page.fileChooserOpened","params":{"mode":"selectSingle","backendNodeId":<id>}}
```

Set the staged path on that backend node:

```bash
node .../cdp-send.js '{"id":3,"method":"DOM.setFileInputFiles","params":{"backendNodeId":<id>,"files":["/workspace/report.pdf"]}}'
```

Pass multiple paths in `files` for a multi-file input (`mode` is
`"selectMultiple"`). Then continue the page's normal flow (e.g. click "Send").

## Credentials and sensitive information (PII)

Never type passwords, API keys, tokens, or any PII on behalf of the user. Instead, open the relevant page and **ask the user to type the sensitive information directly in the browser** — the sandbox browser is visible to them via the Browser tab. Pause and wait for them to confirm before proceeding.

## Stopping the bridge

```bash
kill $(cat /tmp/cdp-bridge.pid)
```
