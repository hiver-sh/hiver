// Browser VM identity, read from browser/config.json (one dir up from these
// scripts) rather than from env. On a warm VM resume the agent process is
// restored with its environment frozen at snapshot-capture time, so per-task env
// never reaches these scripts — a file the app writes per task does.
//
//   sandboxKey     the browser VM's sandbox key (default 'browser-vm')
//   snapshotVmKey  optional VM-state snapshot key; defaults to sandboxKey
const fs = require('fs');
const path = require('path');

function loadBrowserConfig() {
  let cfg = {};
  try {
    cfg = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'config.json'), 'utf8'));
  } catch {}
  const sandboxKey = cfg.sandboxKey || 'browser-vm';
  const snapshotVmKey = cfg.snapshotVmKey || sandboxKey;
  return { sandboxKey, snapshotVmKey };
}

module.exports = { loadBrowserConfig };
