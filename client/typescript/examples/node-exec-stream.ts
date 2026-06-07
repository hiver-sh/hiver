// Run a Node.js function inside the sandbox and stream its output via SSE.
//
// Run with: npx tsx examples/node-exec-stream.ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-node-exec-stream", {
  image: "hiversh/node:alpine",
  entrypoint: "tail -f /dev/null",
  ttl: 0,
});

async function greet(name: string): Promise<void> {
  console.log(`Hello, ${name}!`);
  for (let i = 0; i < 2; i++) {
    await new Promise((resolve) => setTimeout(resolve, 1000));
    console.log(`Hello, ${name} again after ${i + 1}s!`);
  }
  await new Promise((resolve) => setTimeout(resolve, 1000));
  console.error(`Bye!`);
}

function execFunction(
  fn: (...args: any[]) => unknown,
  ...args: unknown[]
): string {
  const serializedFn = fn.toString();
  const serializedArgs = args.map((a) => JSON.stringify(a)).join(", ");
  const script = `(async () => { const fn = ${serializedFn}; await fn(${serializedArgs}); })()`;
  return `node -e '${script}'`;
}

const exec = await sandbox.execStream(execFunction(greet, "world"), {
  cwd: "/workspace",
});

for await (const pipe of exec.pipes) {
  if (pipe.stdout) process.stdout.write("stdout: " + pipe.stdout);
  if (pipe.stderr) process.stderr.write("stderr: " + pipe.stderr);
}

console.info("exit code:", await exec.exitCode);
