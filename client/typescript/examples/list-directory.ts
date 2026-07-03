// List the contents of a sandbox mount. listDirectory returns one entry per
// child (name, agent-visible path, whether it's a directory, and size in
// bytes) and, like the other file calls, bypasses the per-mount ACLs — the
// control-plane is higher privilege than the agent itself.
//
// Run with: npx tsx examples/list-directory.ts
import { createShutdown } from "./utils/index.js";

import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-list-directory-example", {
  image: "node",
});

const { shutdown } = createShutdown(sandbox);

// Seed a couple of files and a subdirectory so the listing has something to show.
await sandbox.writeFile("/workspace/readme.txt", "hello from the host\n");
await sandbox.writeFile(
  "/workspace/data.json",
  JSON.stringify({ ok: true }),
);
await sandbox.exec("mkdir -p /workspace/logs");

async function list(path: string) {
  const entries = await sandbox.listDirectory(path);
  console.info(`${path} has ${entries.length} entries:`);
  for (const e of entries) {
    const kind = e.is_dir ? "dir " : "file";
    console.info(`  ${kind}  ${String(e.size).padStart(6)}  ${e.path}`);
  }
}

await list("/workspace");
await list("/");
await shutdown();
