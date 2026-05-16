// Move a file into a sandbox mount and read it back out. Both calls
// bypass the per-mount ACLs — the control-plane is higher privilege
// than the agent itself.
//
// Run with: npx tsx examples/files.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-example", {
  image: 'mcp-server',
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
});

const written = await sandbox.uploadFile(
  "/workspace",
  "greeting.txt",
  "hello from the host\n",
);
console.info(`uploaded ${written.bytes} bytes → ${written.path}`);

const bytes = await sandbox.downloadFile("/workspace/greeting.txt");
console.info("downloaded:", new TextDecoder().decode(bytes));

void hive.shutdown(sandbox);
