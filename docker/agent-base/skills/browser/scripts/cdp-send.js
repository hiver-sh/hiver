#!/usr/bin/env node
// Usage: node cdp-send.js '{"id":1,"method":"Browser.getVersion","params":{}}'
const net = require('net');

const SOCKET_PATH = '/tmp/cdp.sock';
const command = process.argv[2];

if (!command) {
  process.stderr.write('Usage: node cdp-send.js \'{"id":1,"method":"...","params":{}}\'\n');
  process.exit(1);
}

// The request id we're waiting for a matching response on. CDP replies are
// newline-delimited JSON; large payloads (Page.printToPDF, captureScreenshot)
// span multiple TCP chunks, so we must buffer until a full line arrives rather
// than destroying the socket on the first `data` event.
let requestId;
try {
  requestId = JSON.parse(command).id;
} catch (err) {
  process.stderr.write(`error: invalid JSON command: ${err.message}\n`);
  process.exit(1);
}

const sock = net.connect(SOCKET_PATH, () => {
  sock.write(command.trim() + '\n');
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
      // Not a complete/parseable message yet — skip this line.
      continue;
    }
    // Only the reply to our request has our id; events (with a `method`) and
    // replies to other requests are ignored.
    if (msg.id === requestId) {
      process.stdout.write(line + '\n');
      done = true;
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
