// Call into the HTTP service the sandbox image exposes via the
// /v1/sandbox reverse proxy. `sandbox.getUrl()` is the base URL —
// append paths to it and use any HTTP client (here: plain `fetch`).
// SSE, streaming, and Upgrade (WebSocket) all pass through.
//
// Run with: npx tsx examples/proxy.ts
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

const base = sandbox.getUrl();

// Plain GET — proxied verbatim to the agent service.
const healthz = await fetch(`${base}/healthz`);
console.info("healthz:", healthz.status);

// POST with a JSON body — body, method, headers, status all relayed.
const exec = await fetch(`${base}/exec`, {
  method: "POST",
  headers: { "content-type": "text/plain" },
  body: "echo hello",
});
console.info("exec:", exec.status, await exec.text());

// Streaming response — the proxy disables buffering so chunks arrive
// as the upstream emits them.
const stream = await fetch(`${base}/stream`);
if (stream.body) {
  const reader = stream.body.getReader();
  const decoder = new TextDecoder();
  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    process.stdout.write(decoder.decode(value));
  }
}
