// Mirrors the README walkthrough end-to-end: provision a sandbox,
// stream its events, and keep it alive with periodic pings.
//
// Run with: npx tsx examples/quickstart.ts
import * as hive from "../src";

const sandboxConfig: hive.SandboxConfig = {
  image: "mcp-server",
  ttl: 1800,
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
        host: "go.dev",
        methods: ["GET"],
        paths: ["/solutions/case-studies/*"],
      },
    ],
  },
};

const sandbox = await hive.getOrCreateSandbox("hive-example", sandboxConfig);
console.info("sandbox API server URL:", sandbox.apiServerUrl);
console.info("sandbox URL:", sandbox.url);

const ac = new AbortController();
const events = (async () => {
  for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
    console.info("sandbox event", event);
  }
})();

const ping = setInterval(sandbox.ping, 10_000);

// Stop after 30 seconds.
setTimeout(() => {
  void hive.shutdown(sandbox);
  clearInterval(ping);
  ac.abort();
}, 30_000);

await events.catch(() => {});
