// Run a Node.js function inside the sandbox and stream its output via SSE.
//
// Run with: npx tsx examples/node-exec-stream.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-node-exec-stream", {
  // Built with: ./scripts/bundle-images.sh node:alpine hive-node-sandbox
  image: "hive-node-sandbox",
  entrypoint: "tail -f /dev/null",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  ttl: 0,
});

async function greet(name: string): Promise<void> {
  console.log(`Hello, ${name}!`);
  for (let i = 0; i < 100; i++) {
    await new Promise(resolve => setTimeout(resolve, 1000));
    console.log(`Hello, ${name} again after ${i+1}s!`);
  }
  await new Promise(resolve => setTimeout(resolve, 1000));
  console.error(`Bye!`);
}

function execFunction(fn: (...args: any[]) => unknown, ...args: unknown[]): string {
  const serializedFn = fn.toString();
  const serializedArgs = args.map(a => JSON.stringify(a)).join(", ");
  const script = `(async () => { const fn = ${serializedFn}; await fn(${serializedArgs}); })()`;
  return `node -e '${script}'`;
}

for await (const event of sandbox.execStream(
  execFunction(greet, "world"),
  { cwd: "/workspace" },
)) {
  if (event.type === "stdout") process.stdout.write("stdout: " + event.text);
  else if (event.type === "stderr") process.stderr.write("stderr: " + event.text);
  else console.info("exit code:", event.code);
}

// await hive.shutdown(sandbox);
