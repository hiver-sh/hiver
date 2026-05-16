// Spin up a sandbox running the `mcp-server` image and hand its
// `/v1/sandbox` URL to the MCP Inspector. The inspector connects to
// the MCP server through the sandbox's reverse proxy, so every call
// is mediated by sandboxd's egress + FUSE policies.
//
// Run with: npx tsx examples/mcp-inspector.ts
import { spawn } from "node:child_process";
import process from "node:process";
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-mcp-inspector", {
  image: "mcp-server",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "ro" }],
    },
  ]
});

console.info("MCP inspector → ", sandbox.url);

const child = spawn(
  "npx",
  ["@modelcontextprotocol/inspector", "--server-url", sandbox.url],
  { stdio: "inherit" },
);

// Forward Ctrl-C to the inspector so it can tear itself down before
// we shut the sandbox.
process.on("SIGINT", () => child.kill("SIGINT"));

child.on("exit", async (code) => {
  await hive.shutdown(sandbox);
  process.exit(code ?? 0);
});

for await (const event of sandbox.getEventsStream()) {
  console.info("sandbox event", event);
}
