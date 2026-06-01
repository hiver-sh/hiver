// Run a Python function inside the sandbox and stream its output via SSE.
//
// Run with: npx tsx examples/python-exec-stream.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-python-exec-stream", {
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

const script = `
import sys, time

def greet(name):
    print(f"Hello, {name}!", flush=True)
    time.sleep(0.5)
    print(f"Hello, {name} again after 500ms!", flush=True)
    time.sleep(0.5)
    print("Bye!", file=sys.stderr, flush=True)

greet("world")
`.trim();

for await (const event of sandbox.execStream(
  `python3 -c '${script}'`,
  { cwd: "/workspace" },
)) {
  if (event.type === "stdout") process.stdout.write("stdout: " + event.text);
  else if (event.type === "stderr") process.stderr.write("stderr: " + event.text);
  else console.info("exit code:", event.code);
}

await hive.shutdown(sandbox);
