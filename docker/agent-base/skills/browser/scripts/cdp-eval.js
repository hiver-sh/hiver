#!/usr/bin/env node
// One-shot helper: read a JS file, wrap it in an IIFE, and run it in the page via
// Runtime.evaluate — collapsing the write-file -> build-JSON -> cdp-send dance into
// a single command per DOM action.
//
// Usage:
//   node cdp-eval.js <file.js>              # run the script, print its return value
//   node cdp-eval.js <file.js> <sessionId>  # eval inside an attached target/session
//   echo 'return document.title' | node cdp-eval.js -   # read script from stdin
//
// Pass a <sessionId> once you've done Target.attachToTarget — WITHOUT it, the eval
// runs against the top-level session and, if you meant a page target, silently comes
// back `undefined` (wrong scope, no error). See the browser skill's attach step.
//
// The file's body is wrapped in `(async () => { <body> })()` so:
//   - top-level `const`/`let` are function-scoped and DON'T leak across calls
//     (raw Runtime.evaluate shares one global scope, so a second script reusing a
//     name like `el` throws "Identifier already declared"), and
//   - you can `await` and `return` a value, which is JSON-serialized back to you.
const fs = require('fs');
const net = require('net');

const SOCKET_PATH = '/tmp/cdp.sock';
const arg = process.argv[2];
const sessionId = process.argv[3]; // optional — required after Target.attachToTarget

if (!arg) {
  process.stderr.write('Usage: node cdp-eval.js <file.js> [sessionId]   (use - for stdin)\n');
  process.exit(1);
}

const body = fs.readFileSync(arg === '-' ? 0 : arg, 'utf8');
// Wrap in an async IIFE: scopes declarations and enables await/return.
const expression = `(async () => {\n${body}\n})()`;

const id = 1;
const message = {
  id,
  method: 'Runtime.evaluate',
  params: { expression, awaitPromise: true, returnByValue: true, userGesture: true },
};
// Flattened CDP: a top-level sessionId routes the command to the attached session.
if (sessionId) message.sessionId = sessionId;
const payload = JSON.stringify(message);

const sock = net.connect(SOCKET_PATH, () => {
  sock.write(payload + '\n');
});

let buffer = '';
let done = false;

sock.on('data', (chunk) => {
  buffer += chunk.toString();
  let idx;
  while ((idx = buffer.indexOf('\n')) !== -1) {
    const line = buffer.slice(0, idx);
    buffer = buffer.slice(idx + 1);
    if (!line.trim()) continue;
    let msg;
    try {
      msg = JSON.parse(line);
    } catch (err) {
      continue;
    }
    if (msg.id === id) {
      done = true;
      const res = msg.result || {};
      if (res.exceptionDetails) {
        const ex = res.exceptionDetails;
        const text = (ex.exception && (ex.exception.description || ex.exception.value)) || ex.text;
        process.stderr.write(`page error: ${text}\n`);
        sock.destroy();
        process.exit(1);
      }
      // Print just the returned value (returnByValue gives it under result.value).
      const value = res.result ? res.result.value : undefined;
      process.stdout.write(JSON.stringify(value) + '\n');
      sock.destroy();
      return;
    }
  }
});

sock.on('close', () => {
  if (!done) {
    process.stderr.write('error: socket closed before a matching response was received\n');
    process.exit(1);
  }
});

sock.on('error', (err) => {
  process.stderr.write(`error: ${err.message}\n`);
  process.exit(1);
});
