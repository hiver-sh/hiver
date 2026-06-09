// Uses a custom Docker image and consume the sandbox event stream.
// `getEventsStream` handles resume on its own: if the underlying
// SSE connection drops, it reconnects with the last id observed,
// so no events are missed across a transient blip. The caller
// doesn't track a cursor.
//
// Run with: npx tsx examples/custom-image
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as hiver from "@hiver.sh/client";
import { buildBundle, createShutdown } from "../utils/index.js";
const here = dirname(fileURLToPath(import.meta.url));
const imageTag = "node-example-image-bundle";

console.log(`> Building sandbox bundle ${imageTag}`);
await buildBundle(join(here, "image"), imageTag);

console.log("> Starting sandbox");
const sandbox = await hiver.getOrCreateSandbox("hive-example", {
  image: imageTag,
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  egress: [
    {
      access: "allow",
      host: "github.com",
      paths: ["/hiver-sh/hiver"],
    },
    {
      access: "allow",
      host: "www.google.com",
    },
  ],
});

const { ac, shutdown } = createShutdown(sandbox);

console.log("> Streaming events");
for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  console.info("sandbox event", event);
}

await shutdown();
