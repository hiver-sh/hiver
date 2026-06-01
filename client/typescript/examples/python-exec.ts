// Run a Python function inside the sandbox and print the buffered result.
//
// Run with: npx tsx examples/python-exec.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-python-exec", {
  // Built with: ./scripts/bundle-images.sh python:3.13-alpine hive-python-sandbox
  image: "hive-python-sandbox",
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
  `python3 -c "print('Hello, world!')"`,
  { cwd: "/workspace" },
);

console.info("stdout: " + result.stdout);
if (result.stderr) console.error("stderr: " + result.stderr);
console.info("exit code:", result.exit_code);

await hive.shutdown(sandbox);
