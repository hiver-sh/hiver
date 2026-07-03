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

## Uploading / attaching a file

Headless Chrome has no OS file picker, so clicking an "Attach"/upload button
opens a native dialog that never renders and the flow just hangs. Instead, stage
the file in the sandbox, then click the button as a human would but intercept the
picker over CDP and feed it the staged path.

**Step 1 — stage the file in the sandbox.** `DOM.setFileInputFiles` takes file
*paths on the machine running Chrome*, not bytes, so the file has to exist inside
the browser-vm first. `write-file.js` uploads a local file and prints its
sandbox-visible path:

```bash
node /home/agent/.claude/skills/browser/scripts/write-file.js ./report.pdf
# -> {"status":"ok","path":"/workspace/report.pdf","bytes":12345}
```

**Step 2 — intercept the file chooser.** Turn the native dialog into a CDP event
so clicking the button never opens a real picker:

```bash
node .../cdp-send.js '{"id":1,"method":"Page.enable","params":{}}'
node .../cdp-send.js '{"id":2,"method":"Page.setInterceptFileChooserDialog","params":{"enabled":true}}'
```

**Step 3 — click "Attach" and read the chooser event.** Click the upload button
the normal way (via the accessibility node / a `Runtime.evaluate` click). Chrome
emits a `Page.fileChooserOpened` event instead of opening a dialog; it carries a
`backendNodeId` identifying the `<input type=file>`:

```json
{"method":"Page.fileChooserOpened","params":{"mode":"selectSingle","backendNodeId":<id>}}
```

**Step 4 — set the staged path on that input.** This populates `input.files` and
fires the `change` event, which is what makes the page start the upload — exactly
as if a human had picked the file:

```bash
node .../cdp-send.js '{"id":3,"method":"DOM.setFileInputFiles","params":{"backendNodeId":<id>,"files":["/workspace/report.pdf"]}}'
```

Pass multiple paths in `files` for a multi-file input (`mode` is
`"selectMultiple"`). Then continue the page's normal flow (e.g. click "Send").

**If the `<input type=file>` is already visible in the DOM**, you can skip the
interception and set files on it directly: `DOM.getDocument` →
`DOM.querySelector` with selector `input[type=file]` to get a `nodeId`, then
`setFileInputFiles` with `{"nodeId":<id>,"files":[...]}`.

## Credentials and sensitive information (PII)

Never type passwords, API keys, tokens, or any PII on behalf of the user. Instead, open the relevant page and **ask the user to type the sensitive information directly in the browser** — the sandbox browser is visible to them via the Browser tab. Pause and wait for them to confirm before proceeding.

## Stopping the bridge

```bash
kill $(cat /tmp/cdp-bridge.pid)
```
