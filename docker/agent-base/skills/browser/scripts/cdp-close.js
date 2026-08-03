#!/usr/bin/env node
// Close the browser's page target — call this once you're done with the
// browser for the current task, so the user's browser view closes instead of
// sitting open on the last-visited page. The next cdp-open.js call creates a
// fresh page automatically, so there's no extra teardown/setup needed here.
//
// Usage:
//   node cdp-close.js
//
// Prints {"status":"ok","closed":true|false} — false means no page was open.
const net = require('net');

const SOCKET_PATH = '/tmp/cdp.sock';

let nextId = 1;
const pending = new Map();
let buffer = '';

const sock = net.connect(SOCKET_PATH);

const send = (method, params) => {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    sock.write(JSON.stringify({ id, method, params: params || {} }) + '\n');
  });
};

sock.on('data', (chunk) => {
  buffer += chunk.toString();
  let idx;
  while ((idx = buffer.indexOf('\n')) !== -1) {
    const line = buffer.slice(0, idx);
    buffer = buffer.slice(idx + 1);
    if (!line.trim()) continue;
    let msg;
    try { msg = JSON.parse(line); } catch { continue; }
    if (msg.id !== undefined && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      if (msg.error) reject(new Error(msg.error.message));
      else resolve(msg.result);
    }
  }
});

sock.on('error', (err) => {
  process.stderr.write(`error: ${err.message}\n`);
  process.exit(1);
});

async function main() {
  await new Promise((resolve, reject) => {
    sock.once('connect', resolve);
    sock.once('error', reject);
  });

  const { targetInfos } = await send('Target.getTargets');
  const page = (targetInfos || []).find((t) => t.type === 'page');
  if (!page) {
    process.stdout.write(JSON.stringify({ status: 'ok', closed: false }) + '\n');
    sock.destroy();
    return;
  }

  await send('Target.closeTarget', { targetId: page.targetId });
  process.stdout.write(JSON.stringify({ status: 'ok', closed: true }) + '\n');
  sock.destroy();
}

main().catch((err) => {
  process.stderr.write(`error: ${err.message}\n`);
  process.exit(1);
});
