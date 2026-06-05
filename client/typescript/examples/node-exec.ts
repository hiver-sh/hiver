// Run a Node.js function inside the sandbox and print the buffered result.
//
// Run with: npx tsx examples/node-exec.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-node-exec", {
  image: "hive-node-sandbox",
  entrypoint: "tail -f /dev/null",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
});


const result = await sandbox.exec(
  `node -e "console.log('Hello, world!')"`,
  { cwd: "/workspace" },
);

console.info("stdout: " + result.stdout);
if (result.stderr) console.error("stderr: " + result.stderr);
console.info("exit code:", result.exit_code);

await hive.shutdown(sandbox);
