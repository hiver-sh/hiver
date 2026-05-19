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
        host: 'www.google.com'
      },
      ...hive.allowedPythonPackages('numpy'),
    ]
  }
});

console.info("MCP inspector → ", sandbox.url);

const mcpInspector = spawn(
  "npx",
  ["@modelcontextprotocol/inspector", "--server-url", sandbox.url],
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
