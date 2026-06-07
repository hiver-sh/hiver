// Spin up a sandbox running the local `mcp-server` image and hand its
// exposed port to the MCP Inspector.
//
// Run with: npx tsx examples/mcp-server
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import process from "node:process";
import * as hive from "../../src";

const here = dirname(fileURLToPath(import.meta.url));
const imageTag = "mcp-server-image-bundle";
const MCP_PORT = 3000;

console.log(`> Building sandbox bundle ${imageTag}`);
await buildBundle(join(here, "image"), imageTag);

console.log("> Starting sandbox");
const sandbox = await hive.getOrCreateSandbox("hive-mcp-server", {
  image: imageTag,
  ttl: 0,
  fs: [{ backend: "local", mount: "/workspace" }],
});

const mcpURL = `${sandbox.proxyUrl(MCP_PORT)}/mcp`;
console.info(`> MCP inspector → ${mcpURL}`);

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

function buildBundle(sandboxImage: string, bundleTag: string): Promise<void> {
  return spawnOk("hiver", ["bundle", sandboxImage, "--tag", bundleTag]);
}

function spawnOk(cmd: string, args: string[]): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { stdio: "inherit" });
    child.once("error", reject);
    child.once("exit", (code: number | null) =>
      code === 0
        ? resolve()
        : reject(new Error(`${cmd} ${args[0]}: exit ${code}`)),
    );
  });
}
