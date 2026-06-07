// Move a file into a sandbox mount and read it back out. Both calls
// bypass the per-mount ACLs — the control-plane is higher privilege
// than the agent itself.
//
// Run with: npx tsx examples/files.ts
import * as hive from "../src";
import { createShutdown } from "./shutdown.js";

const sandbox = await hive.getOrCreateSandbox("hive-files-example", {
  image: "hiversh/node:alpine",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
});

const { shutdown } = createShutdown(sandbox);

const written = await sandbox.uploadFile(
  "/workspace",
  "greeting.txt",
  "hello from the host\n",
);
console.info(`uploaded ${written.bytes} bytes → ${written.path}`);

const bytes = await sandbox.downloadFile("/workspace/greeting.txt");
console.info("downloaded:", new TextDecoder().decode(bytes));

await shutdown();
