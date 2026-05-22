// Spin up a sandbox running the `mcp-server` image and hand its
// exposed endpoint URL to the MCP Inspector. The inspector connects
// directly to the MCP server's published port.
//
// Run with: npx tsx examples/mcp-inspector.ts
import { spawn } from "node:child_process";
import process from "node:process";
import * as hive from "../src";

const sandbox = await hive.getOrCreateSandbox("hive-mcp-inspector", {
  ttl: 0,
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "ro" }],
    },
  ],
  egress: {
    allow: [
      {
        host: "www.google.com",
      },
      {
        host: "api.anthropic.com",
      },
      ...hive.allowedPythonPackages("numpy"),
    ],
  },
});

if (!sandbox.exposedEndpoint) {
  console.error("sandbox image has no EXPOSE port; cannot connect MCP Inspector");
  process.exit(1);
}
const mcpURL = `http://${sandbox.exposedEndpoint}`;
console.info("MCP inspector → ", mcpURL);

const mcpInspector = spawn(
  "npx",
  ["@modelcontextprotocol/inspector", "--server-url", mcpURL],
  { stdio: "inherit" },
);

const ac = new AbortController();
async function shutdown(code: number) {
  if (ac.signal.aborted) return;
  ac.abort();
  mcpInspector.kill("SIGINT");
  await hive.shutdown(sandbox);
  process.exit(code);
}

process.once("SIGINT", () => shutdown(130));
process.once("SIGTERM", () => shutdown(143));
mcpInspector.on("exit", (code: number | null) => shutdown(code ?? 0));

for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  console.info("sandbox event", event);
}
