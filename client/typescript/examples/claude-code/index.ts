// Uses a custom Docker image and consume the sandbox event stream.
// `getEventsStream` handles resume on its own: if the underlying
// SSE connection drops, it reconnects with the last id observed,
// so no events are missed across a transient blip. The caller
// doesn't track a cursor.
//
// Run with: npx tsx examples/claude-code
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as hive from "../../src";
import { createShutdown } from "../shutdown.js";

if (!process.env.ANTHROPIC_API_KEY) {
  console.error("ANTHROPIC_API_KEY must be defined");
  process.exit(1);
}

const here = dirname(fileURLToPath(import.meta.url));
const sourceImage = "hive-example-claude-worker";
const imageTag = "hive-example-claude-worker-bundle";
const scriptPath = join(here, "../../../../scripts/bundle-images.sh");

console.log(`> Building image ${sourceImage}`);
await buildImage(sourceImage, join(here, "image"));

console.log(`> Building sandbox bundle ${imageTag}`);
await buildBundle(scriptPath, sourceImage, imageTag);

console.log("> Starting sandbox");
const sandbox = await hive.getOrCreateSandbox("hive-claude-sdk-worker-2", {
  image: imageTag,
  env: {
    ANTHROPIC_API_KEY: "<private>", // Don't expose the key to the agent
  },
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
        host: "api.anthropic.com",
        override: {
          headers: {
            "x-api-key": process.env.ANTHROPIC_API_KEY,
          },
        },
      },
    ],
  },
});

spawn("npx", ["@modelcontextprotocol/inspector", "--server-url", sandbox.url], {
  stdio: "inherit",
});

const { ac, shutdown } = createShutdown(sandbox);

console.log("> Streaming events");
for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
  console.info("sandbox event", event);
}

await shutdown();

function buildImage(tag: string, contextDir: string): Promise<void> {
  return spawnOk("docker", ["build", "-t", tag, contextDir]);
}

function buildBundle(
  scriptPath: string,
  sandboxImage: string,
  bundleTag: string,
): Promise<void> {
  return spawnOk("bash", [scriptPath, sandboxImage, bundleTag]);
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
