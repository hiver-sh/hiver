// Persist a sandbox's filesystem across a full shutdown using snapshots.
//
// The files snapshot is captured automatically before the sandbox shuts down
// (when `write_on_shutdown` is set) and restored before it next starts (both
// keyed by `files.key`). Here we write a file into the sandbox's own filesystem
// — not a host-backed mount — so the only way it survives the shutdown is the
// snapshot.
//
// Run with: npm install && npm start
import * as hiver from "@hiver.sh/client";

const ID = "hiver-snapshot-example";
const KEY = "snapshot-example";

// Snapshot config shared by both boots: capture /root on shutdown, restore it on start.
const snapshot: hiver.Snapshot = {
  files: { key: KEY, write_on_shutdown: true, include: ["/root/**"] },
};
const config: hiver.SandboxConfig = {
  image: "node",
  snapshot,
};

// --- First boot: write a file, then shut down (captures the snapshot). ---
const first = await hiver.getOrCreateSandbox(ID, config);

const before = await first.exec(
  "echo 'hello from the first boot' > /root/note.txt && cat /root/note.txt",
);
console.info("wrote /root/note.txt:", before.stdout.trim());

console.info("shutting down (capturing snapshot)…");
await first.shutdown();

// --- Second boot: same files.key brings /root/note.txt back. ---
const second = await hiver.getOrCreateSandbox(ID, config);

const after = await second.exec("cat /root/note.txt");
console.info("restored /root/note.txt:", after.stdout.trim());

if (after.exit_code !== 0) {
  console.error("expected the file to be restored from the snapshot");
  process.exitCode = 1;
}

await second.shutdown();
