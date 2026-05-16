// Consume the sandbox event stream. `getEventsStream` handles
// resume on its own: if the underlying SSE connection drops, it
// reconnects with the last id observed, so no events are missed
// across a transient blip. The caller doesn't track a cursor.
//
// Run with: npx tsx examples/resume-events.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-example", {
  image: "mcp-server",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
});

for await (const event of sandbox.getEventsStream()) {
  console.info("sandbox event", event);
}
