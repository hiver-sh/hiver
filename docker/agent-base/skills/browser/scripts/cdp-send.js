#!/usr/bin/env node
// Usage: node cdp-send.js '{"id":1,"method":"Browser.getVersion","params":{}}'
const net = require('net');

const SOCKET_PATH = '/tmp/cdp.sock';
const command = process.argv[2];

if (!command) {
  process.stderr.write('Usage: node cdp-send.js \'{"id":1,"method":"...","params":{}}\'\n');
  process.exit(1);
}

const sock = net.connect(SOCKET_PATH, () => {
  sock.write(command.trim() + '\n');
});

sock.on('data', (data) => {
  process.stdout.write(data.toString());
  sock.destroy();
});

sock.on('error', (err) => {
  process.stderr.write(`error: ${err.message}\n`);
  process.exit(1);
});
