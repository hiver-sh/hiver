// Run a Node.js function inside the sandbox and print the buffered result.
//
// Run with: npx tsx examples/node-exec.ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-node-exec", {
  image: "hiversh/node:alpine",
  entrypoint: "tail -f /dev/null",
});

const result = await sandbox.exec(`node -e "console.log('Hello, world!')"`, {
  cwd: "/workspace",
});

console.info("stdout: " + result.stdout);
if (result.stderr) console.error("stderr: " + result.stderr);
console.info("exit code:", result.exit_code);

await hiver.shutdown(sandbox);
