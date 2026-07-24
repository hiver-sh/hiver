#!/usr/bin/env node
// One-shot "open a URL" helper: connect to the bridge, attach to the page target,
// navigate, wait for the load event, and print the page's sessionId — collapsing
// the start-bridge -> getTargets -> attachToTarget -> navigate dance (four agent
// turns, four LLM round-trips) into a single command.
//
// Usage:
//   node cdp-open.js <url>            # navigate the first page target to <url>
//   node cdp-open.js <url> --wait 30  # override the load-wait timeout (seconds)
//
// Prints: {"sessionId":"<SID>","url":"<final-or-requested-url>"}
// Reuse that <SID> with cdp-eval.js / cdp-send.js for every later interaction —
// you do NOT need a separate Target.attachToTarget step.
//
// It retries the socket connect, so it can run immediately after launching
// cdp-bridge.js in the background (no separate "wait for ready" step needed):
//   node cdp-bridge.js > /tmp/cdp-bridge.log 2>&1 & node cdp-open.js "<url>"
const net = require('net');

const SOCKET_PATH = '/tmp/cdp.sock';
const url = process.argv[2];
const waitIdx = process.argv.indexOf('--wait');
const LOAD_TIMEOUT_MS = (waitIdx !== -1 ? Number(process.argv[waitIdx + 1]) : 30) * 1000;

if (!url || url.startsWith('--')) {
  process.stderr.write('Usage: node cdp-open.js <url> [--wait <seconds>]\n');
  process.exit(1);
}

// How long to keep retrying the socket while the bridge comes up (it must create
// the unix socket and connect its upstream WebSocket first).
const CONNECT_DEADLINE_MS = Date.now() + 20000;

function connect() {
  return new Promise((resolve, reject) => {
    const attempt = () => {
      const sock = net.connect(SOCKET_PATH);
      sock.once('connect', () => resolve(sock));
      sock.once('error', (err) => {
        sock.destroy();
        const retryable = err.code === 'ECONNREFUSED' || err.code === 'ENOENT';
        if (retryable && Date.now() < CONNECT_DEADLINE_MS) {
          setTimeout(attempt, 250);
        } else {
          reject(err);
        }
      });
    };
    attempt();
  });
}

async function main() {
  const sock = await connect();

  // Correlate replies by id and dispatch events by method over the one broadcast
  // connection the bridge gives us. Ids are ours; the page target's sessionId is
  // discovered from the attach reply and threaded onto later commands.
  let nextId = 1;
  const pending = new Map(); // id -> resolve
  const eventWaiters = []; // { match(msg) -> bool, resolve }
  let buffer = '';

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
        const resolve = pending.get(msg.id);
        pending.delete(msg.id);
        resolve(msg);
      } else if (msg.method) {
        for (let i = eventWaiters.length - 1; i >= 0; i--) {
          if (eventWaiters[i].match(msg)) {
            eventWaiters.splice(i, 1)[0].resolve(msg);
          }
        }
      }
    }
  });
  sock.on('error', (err) => { process.stderr.write(`error: ${err.message}\n`); process.exit(1); });

  const send = (method, params, sessionId) => {
    const id = nextId++;
    const message = { id, method, params: params || {} };
    if (sessionId) message.sessionId = sessionId;
    return new Promise((resolve) => {
      pending.set(id, resolve);
      sock.write(JSON.stringify(message) + '\n');
    });
  };

  const waitEvent = (method, sessionId, timeoutMs) =>
    new Promise((resolve) => {
      const waiter = {
        match: (m) => m.method === method && (!sessionId || m.sessionId === sessionId),
        resolve: (m) => { clearTimeout(timer); resolve(m); },
      };
      const timer = setTimeout(() => {
        const i = eventWaiters.indexOf(waiter);
        if (i !== -1) eventWaiters.splice(i, 1);
        resolve(null); // navigation was issued; the page just didn't finish loading
      }, timeoutMs);
      eventWaiters.push(waiter);
    });

  // 1. find the page target
  const targets = await send('Target.getTargets');
  const infos = (targets.result && targets.result.targetInfos) || [];
  const page = infos.find((t) => t.type === 'page');
  if (!page) {
    process.stderr.write('error: no page target found (is the browser up?)\n');
    process.exit(1);
  }

  // 2. attach (flatten) -> sessionId, threaded onto every later command
  const attached = await send('Target.attachToTarget', { targetId: page.targetId, flatten: true });
  const sessionId = attached.result && attached.result.sessionId;
  if (!sessionId) {
    process.stderr.write('error: attach did not return a sessionId\n');
    process.exit(1);
  }

  // 3. enable Page events so loadEventFired reaches us, then navigate
  await send('Page.enable', {}, sessionId);
  const loaded = waitEvent('Page.loadEventFired', sessionId, LOAD_TIMEOUT_MS);
  const nav = await send('Page.navigate', { url }, sessionId);
  if (nav.result && nav.result.errorText) {
    process.stderr.write(`error: navigation failed: ${nav.result.errorText}\n`);
    process.exit(1);
  }
  await loaded;

  process.stdout.write(JSON.stringify({ sessionId, url }) + '\n');
  sock.destroy();
}

main().catch((err) => {
  process.stderr.write(`error: ${err.message}\n`);
  process.exit(1);
});
