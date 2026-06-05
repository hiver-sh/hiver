// Persist a sandbox's filesystem across a full shutdown using snapshots.
//
// A snapshot is captured automatically before the sandbox shuts down (under
// `write_key`) and restored before it next starts (from `restore_key`). Here we
// write a file into the sandbox's own filesystem — not a host-backed mount — so
// the only way it survives the shutdown is the snapshot.
//
// Run with: npx tsx examples/snapshot.ts
import * as hive from "../src";

const ID = "hive-snapshot-example";
const KEY = "snapshot-example";

// Snapshot config shared by both boots: capture /root on shutdown, restore it on start.
const snapshot = { restore_key: KEY, write_key: KEY, include: ["/root/**"] };
const config: hive.SandboxConfig = {
  image: "hive-node-sandbox",
  isolation: 'microvm',
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  snapshot,
};

// --- First boot: write a file, then shut down (captures the snapshot). ---
const first = await hive.getOrCreateSandbox(ID, config);

const before = await first.exec(
  "echo 'hello from the first boot' > /root/note.txt && cat /root/note.txt",
);
console.info("wrote /root/note.txt:", before.stdout.trim());

console.info("shutting down (capturing snapshot)…");
await hive.shutdown(first);

// --- Second boot: same restore_key brings /root/note.txt back. ---
const second = await hive.getOrCreateSandbox(ID, config);

const after = await second.exec("cat /root/note.txt");
console.info("restored /root/note.txt:", after.stdout.trim());

if (after.exit_code !== 0) {
  console.error("expected the file to be restored from the snapshot");
  process.exitCode = 1;
}

await hive.shutdown(second);
