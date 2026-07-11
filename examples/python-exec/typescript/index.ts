// Run a Python function inside the sandbox and print the buffered result.
//
// Run with: npm install && npm start
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-python-exec", {
  image: "python",
});

const result = await sandbox.exec(["python3", "-c", "print('Hello, world!')"], {
  cwd: "/workspace",
});

console.info("stdout: " + result.stdout);
if (result.stderr) console.error("stderr: " + result.stderr);
console.info("exit code:", result.exit_code);
