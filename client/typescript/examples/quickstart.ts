// Mirrors the README walkthrough end-to-end: provision a sandbox,
// stream its events, and keep it alive with periodic pings.
//
// Run with: npx tsx examples/quickstart.ts
import * as hive from "../src";
import { createShutdown } from "./shutdown.js";

const sandboxConfig: hive.SandboxConfig = {
  ttl: 1800,
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
      host: "go.dev",
      methods: ["GET"],
      paths: ["/solutions/case-studies/*"],
    },
  ],
};

const sandbox = await hive.getOrCreateSandbox("hive-example", sandboxConfig);
console.info("sandbox endpoint:", `http://${sandbox.exposedEndpoint}`);

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
setTimeout(() => shutdown(0), 30_000);

await events.catch(() => {});
