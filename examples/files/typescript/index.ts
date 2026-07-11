// Move a file into a sandbox mount and read it back out. Both calls
// bypass the per-mount ACLs — the control-plane is higher privilege
// than the agent itself.
//
// Run with: npm install && npm start
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hive-files-example", {
  image: "node",
});

const written = await sandbox.writeFile(
  "/workspace/greeting.txt",
  "hello from the host\n",
);
console.info(`uploaded ${written.bytes} bytes → ${written.path}`);

const bytes = await sandbox.readFile("/workspace/greeting.txt");
console.info("downloaded:", new TextDecoder().decode(bytes));
