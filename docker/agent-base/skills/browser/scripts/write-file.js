#!/usr/bin/env node
// Stage a local file into the browser-vm sandbox filesystem so Chrome can read
// it for uploads (DOM.setFileInputFiles takes paths on the machine running
// Chrome, not bytes — so the file must exist inside the sandbox first).
//
// Usage:
//   node write-file.js <local-path> [dest-basename]
//
// Prints the sandbox-visible path (e.g. /workspace/report.pdf) as JSON; feed
// that path straight into DOM.setFileInputFiles.
const fs = require('fs');
const path = require('path');

const { getOrCreateSandbox } = require('@hiver.sh/client');

// Mount the file lands under. Must match a configured fs[].mount on the browser
// image; /workspace is the default agent-visible mount.
const DESTINATION = '/workspace';

async function main() {
  const src = process.argv[2];
  if (!src) {
    process.stderr.write('Usage: node write-file.js <local-path> [dest-basename]\n');
    process.exit(1);
  }
  const filename = process.argv[3] || path.basename(src);

  const content = fs.readFileSync(src);

  const sandbox = await getOrCreateSandbox('browser-vm', {
    image: 'browser',
    snapshot: { vm: { key: 'browser-vm' } }
  });

  const destPath = path.posix.join(DESTINATION, filename);
  const { path: dest, bytes } = await sandbox.writeFile(destPath, content);
  process.stdout.write(JSON.stringify({ status: 'ok', path: dest, bytes }) + '\n');
}

main().catch((err) => {
  process.stderr.write(`[write-file] fatal: ${err.message}\n`);
  process.exit(1);
});
