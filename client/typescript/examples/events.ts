// Consume the sandbox event stream. `getEventsStream` handles
// resume on its own: if the underlying SSE connection drops, it
// reconnects with the last id observed, so no events are missed
// across a transient blip. The caller doesn't track a cursor.
//
// Run with: npx tsx examples/events.ts
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as hive from "../src";
import { createShutdown } from "./shutdown.js";

// Build the image the controller will spawn by tag. Each run gets a
// fresh `:<timestamp>` tag so the sandbox always picks up the latest
// build instead of a cached `:latest` from a previous run.
const here = dirname(fileURLToPath(import.meta.url));
const imageTag = `node-example-image:${Date.now()}`;

console.log(`> Building image ${imageTag}`);
await buildImage(imageTag, join(here, "node-example-image"));

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
  return new Promise((resolve, reject) => {
    const child = spawn("docker", ["build", "-t", tag, contextDir], {
      stdio: "inherit",
    });
    child.once("error", reject);
    child.once("exit", (code: number | null) =>
      code === 0
        ? resolve()
        : reject(new Error(`docker build ${tag}: exit ${code}`)),
    );
  });
}
