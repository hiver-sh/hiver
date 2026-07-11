// Spin up a sandbox running the local `mcp-server` image and hand its
// exposed port to the MCP Inspector.
//
// Run with: npm install && npm run build && npm start
import { spawn } from "node:child_process";
import * as hiver from "@hiver.sh/client";

const MCP_PORT = 3000;

// Build the image first with `npm run build` (hiver bundle ./image).
const sandbox = await hiver.getOrCreateSandbox("hive-mcp-server", {
  image: "mcp-server-image-bundle",
  ttl: 0,
  fs: [{ backend: "local", mount: "/workspace" }],
});

const mcpURL = `${sandbox.proxyUrl(MCP_PORT)}mcp`;
console.info(`> MCP inspector → ${mcpURL}`);

const mcpInspector = spawn(
  "npx",
  ["@modelcontextprotocol/inspector", "--server-url", mcpURL],
  { stdio: "inherit" },
);

// When the inspector exits, stop consuming the event stream so this process
// ends too. The sandbox is left running — stop it with `hiver stop hive-mcp-server`.
const ac = new AbortController();
mcpInspector.on("exit", () => ac.abort());

for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  console.info("sandbox event", event);
}
