#!/usr/bin/env node
const fs = require('fs');
const net = require('net');
const { execSync } = require('child_process');

const { getOrCreateSandbox } = require('@hiver.sh/client');
const WebSocket = require('ws');

const SOCKET_PATH = '/tmp/cdp.sock';
try { fs.unlinkSync(SOCKET_PATH); } catch {}

async function main() {
  const sandbox = await getOrCreateSandbox('browser-vm', {
    image: 'browser',
    snapshot: { vm: { key: 'browser-vm' } }
  });

  const wsUrl = sandbox.proxyUrl(9223).replace(/^http/, 'ws') + '/cdp';
  process.stderr.write(`[cdp-bridge] connecting to ${wsUrl}\n`);

  async function connectWithRetry() {
    const MAX_DELAY = 30000;
    let delay = 500;
    let attempt = 0;
    while (true) {
      attempt++;
      try {
        const ws = new WebSocket(wsUrl);
        await new Promise((resolve, reject) => {
          ws.once('open', resolve);
          ws.once('unexpected-response', (_req, res) => {
            ws.terminate();
            reject(Object.assign(new Error(`HTTP ${res.statusCode}`), { statusCode: res.statusCode }));
          });
          ws.once('error', reject);
        });
        return ws;
      } catch (err) {
        const isRetryable = err.statusCode === 502 || err.statusCode === 503 || err.statusCode === 504 || err.code === 'ECONNREFUSED';
        process.stderr.write(`[cdp-bridge] connect attempt ${attempt} failed (${err.message})${isRetryable ? `, retrying in ${delay}ms` : ''}\n`);
        if (!isRetryable) throw err;
        await new Promise(r => setTimeout(r, delay));
        delay = Math.min(delay * 2, MAX_DELAY);
      }
    }
  }

  const ws = await connectWithRetry();
  process.stderr.write('[cdp-bridge] connected\n');

  const clients = new Set();

  ws.on('message', (data) => {
    const line = data.toString() + '\n';
    for (const c of clients) {
      try { c.write(line); } catch {}
    }
  });

  ws.on('close', () => { process.stderr.write('[cdp-bridge] WS closed\n'); process.exit(0); });
  ws.on('error', (err) => process.stderr.write(`[cdp-bridge] WS error: ${err.message}\n`));

  const server = net.createServer((socket) => {
    clients.add(socket);
    let buf = '';
    socket.on('data', (chunk) => {
      buf += chunk.toString();
      const lines = buf.split('\n');
      buf = lines.pop();
      for (const line of lines) {
        const trimmed = line.trim();
        if (trimmed) ws.send(trimmed);
      }
    });
    socket.on('close', () => clients.delete(socket));
    socket.on('error', () => clients.delete(socket));
  });

  server.listen(SOCKET_PATH, () => {
    process.stderr.write(`[cdp-bridge] ready on ${SOCKET_PATH}\n`);
    process.stdout.write(JSON.stringify({ status: 'ready', socket: SOCKET_PATH, wsUrl }) + '\n');
  });
}

main().catch((err) => {
  process.stderr.write(`[cdp-bridge] fatal: ${err.message}\n`);
  process.exit(1);
});
