// Run a Node.js function inside the sandbox and print the buffered result.
//
// Run with: npx tsx examples/node-exec.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("claude", {
  image: "hiveruntime/agent-cli:latest",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
});


const result = await sandbox.exec("claude -p 'Write a poem and save it as pdf'");
console.log(result.stdout);

console.info("stdout: " + result.stdout);
if (result.stderr) console.error("stderr: " + result.stderr);
console.info("exit code:", result.exit_code);

await hive.shutdown(sandbox);
