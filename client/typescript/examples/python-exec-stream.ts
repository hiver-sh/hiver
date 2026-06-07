// Run a Python function inside the sandbox and stream its output via SSE.
//
// Run with: npx tsx examples/python-exec-stream.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-python-exec-stream", {
  image: "hiversh/python:3.13-alpine",
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

const exec = await sandbox.execStream(`python3 -c '${script}'`, {
  cwd: "/workspace",
});

for await (const pipe of exec.pipes) {
  if (pipe.stdout) process.stdout.write("stdout: " + pipe.stdout);
  if (pipe.stderr) process.stderr.write("stderr: " + pipe.stderr);
}

console.info("exit code:", await exec.exitCode);
