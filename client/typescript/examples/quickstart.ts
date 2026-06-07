// Mirrors the README walkthrough end-to-end: provision a sandbox,
// stream its events, and keep it alive with periodic pings.
//
// Run with: npx tsx examples/quickstart.ts
import { createShutdown } from "./utils/index.js";

import * as hiver from "@hiver.sh/client";

const sandboxConfig: hiver.SandboxConfig = {
  ttl: 1800,
  egress: [
    {
      access: "allow",
      host: "go.dev",
      methods: ["GET"],
      paths: ["/solutions/case-studies/*"],
    },
  ],
};

const sandbox = await hiver.getOrCreateSandbox(
  "hiver-quickstart",
  sandboxConfig,
);

let ping: ReturnType<typeof setInterval>;
const { ac, shutdown } = createShutdown(sandbox, {
  cleanup: () => clearInterval(ping),
});

const events = (async () => {
  for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
    console.info("sandbox event", event);
  }
})();

ping = setInterval(sandbox.ping, 10_000);

// Stop after 30 seconds.
setTimeout(() => shutdown(0), 30_000 * 100);

await events.catch(() => {});
