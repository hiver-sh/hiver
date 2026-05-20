// Uses a custom Docker image and consume the sandbox event stream.
// `getEventsStream` handles resume on its own: if the underlying
// SSE connection drops, it reconnects with the last id observed,
// so no events are missed across a transient blip. The caller
// doesn't track a cursor.
//
// Run with: npx tsx examples/custom-image
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as hive from "../../src";
import { createShutdown } from "../shutdown.js";

const here = dirname(fileURLToPath(import.meta.url));
const sourceImage = 'hive-node-example-image';
const imageTag = 'node-example-image-bundle';
const scriptPath = join(here, "../../../../scripts/bundle-images.sh");

console.log(`> Building image ${sourceImage}`);
await buildImage(sourceImage, join(here, "image"));

console.log(`> Building sandbox bundle ${imageTag}`);
await buildBundle(scriptPath, sourceImage, imageTag);

console.log('> Starting sandbox');
const sandbox = await hive.getOrCreateSandbox("hive-example", {
  image: imageTag,
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  egress: {
    allow: [
      {
        host: 'github.com',
        paths: ['/blasten/hive']
      },
      {
        host: 'www.google.com'
      }
    ]
  }
});

const { ac, shutdown } = createShutdown(sandbox);

console.log('> Streaming events');
for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  console.info("sandbox event", event);
}

await shutdown();

function buildImage(tag: string, contextDir: string): Promise<void> {
  return spawnOk("docker", ["build", "-t", tag, contextDir]);
}

function buildBundle(scriptPath: string, sandboxImage: string, bundleTag: string): Promise<void> {
  return spawnOk("bash", [scriptPath, sandboxImage, bundleTag]);
}

function spawnOk(cmd: string, args: string[]): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { stdio: "inherit" });
    child.once("error", reject);
    child.once("exit", (code: number | null) =>
      code === 0
        ? resolve()
        : reject(new Error(`${cmd} ${args[0]}: exit ${code}`)),
    );
  });
}
