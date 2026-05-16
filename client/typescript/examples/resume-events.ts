// Track the last event id as we consume the stream, and on a
// transient disconnect reopen the stream from that cursor. The server
// replays every event with a greater id, then continues with new
// ones, so nothing is missed.
//
// The cursor lives in memory — fine for a long-running process. If
// the process itself can restart, persist `lastEventId` somewhere
// durable instead (database, file, etc.) and seed it on boot.
//
// Run with: npx tsx examples/resume-events.ts
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-example", {
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
});

let lastEventId: number | undefined;

// Retry forever, resuming from wherever we left off. A real consumer
// would back off and give up after some bound; this loop is just here
// to show the resume contract.
while (true) {
  try {
    for await (const event of sandbox.getEventsStream({ lastEventId })) {
      console.info(event.type, event.id, event.timestamp);
      lastEventId = event.id;
    }
    // Stream ended cleanly (server shut down) — stop.
    break;
  } catch (err) {
    console.warn("stream dropped, resuming after id", lastEventId, err);
    await new Promise((r) => setTimeout(r, 1_000));
  }
}
