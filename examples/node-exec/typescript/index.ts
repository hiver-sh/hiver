// Run a Node.js function inside the sandbox and print the buffered result.
//
// Run with: npm install && npm start
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-node-exec", {
  image: "node",
});

const result = await sandbox.exec(
  ["node", "-e", "console.log('Hello, world!')"],
  {
    cwd: "/workspace",
  },
);

console.info("stdout: " + result.stdout);
if (result.stderr) console.error("stderr: " + result.stderr);
console.info("exit code:", result.exit_code);
