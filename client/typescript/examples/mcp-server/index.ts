// Spin up a sandbox running the local `mcp-server` image and hand its
// exposed port to the MCP Inspector.
//
// Run with: npx tsx examples/mcp-server/index.ts
import { spawn } from "node:child_process";
import process from "node:process";
import * as hive from "../../src";

const MCP_PORT = 3000;

const sandbox = await hive.getOrCreateSandbox("hive-mcp-server", {
  image: "./examples/mcp-server/image",
  ttl: 0,
  fs: [
    {
      backend: "local",
      mount: "/workspace",
    },
  ],
});

const mcpURL = sandbox.proxyUrl(MCP_PORT) + "/mcp";
console.info("MCP inspector →", mcpURL);

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
