// Provision a sandbox with a single egress allow-rule, subscribe to its
// event stream, then make two outbound requests from inside the sandbox —
// one to the allowed host and one to a blocked host — and print the
// `egress.request` / `egress.response` events the proxy emits for each.
//
// Useful for confirming end-to-end that egress events are flowing (e.g.
// against a remote gateway): point it at your deployment with
//   HIVER_GATEWAY_URL=http://<gateway-host>:10000 npx tsx examples/egress-events.ts
//
// Run with: npx tsx examples/egress-events.ts
import * as hiver from "@hiver.sh/client";

// Default matches DEFAULT_GATEWAY_URL; override to reach a remote gateway.
const gatewayUrl = process.env.HIVER_GATEWAY_URL ?? "http://localhost:10000";

const sandboxConfig: hiver.SandboxConfig = {
  image: "hiversh/node:alpine-microvm",
  // Allow GET to example.com; everything else (e.g. example.org below) is
  // denied by default and should surface as an `egress.request` with
  // access: "denied".
  egress: [
    {
      access: "allow",
      host: "example.com",
      methods: ["GET"],
      paths: ["/*"],
    },
  ],
};

const start = performance.now();
const sandbox = await hiver.getOrCreateSandbox(
  "hiver-egress-events",
  sandboxConfig,
  // Cold starts on a remote cluster (image pull, pod scheduling, sandboxd
  // bring-up, gateway DNS) routinely exceed the default reachability window,
  // so allow longer. A warm sandbox is reused well before this.
  { gatewayUrl, timeoutMs: 120_000 },
);
console.info(
  `getOrCreateSandbox returned in ${(performance.now() - start).toFixed(0)}ms`,
);

const ac = new AbortController();

// Consume the event stream in the background, printing only egress events.
let sawEgress = false;
const events = (async () => {
  for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
    switch (event.type) {
      case "egress.request":
        sawEgress = true;
        console.info(
          `→ egress.request  [${event.access}]  ${event.method} ${event.host}${event.path}`,
        );
        break;
      case "egress.response":
        console.info(
          `← egress.response status=${event.status} duration=${event.duration_ms}ms (req #${event.request_id})`,
        );
        break;
      default:
        // Ignore stdio/exec/resource.usage/etc. for this example.
        break;
    }
  }
})();

console.info("--- fetching allowed host (example.com) ---");
const allowed = await sandbox.exec(
  `node -e "fetch('https://example.com/').then(r => console.log('status', r.status)).catch(e => console.log('error', e.message))"`,
  { cwd: "/workspace" },
);
console.info("allowed exec:", allowed.stdout.trim() || allowed.stderr.trim());

console.info("--- fetching blocked host (example.org) ---");
const blocked = await sandbox.exec(
  `node -e "fetch('https://example.org/').then(r => console.log('status', r.status)).catch(e => console.log('error', e.message))"`,
  { cwd: "/workspace" },
);
console.info("blocked exec:", blocked.stdout.trim() || blocked.stderr.trim());

console.info(
  sawEgress
    ? "\n✓ egress events are flowing"
    : "\n✗ no egress events observed — nothing reached the proxy",
);

ac.abort();
