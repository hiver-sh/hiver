// Demonstrates proxying HTTP requests to a service running inside the sandbox.
// The sandbox image runs two echo servers (EXPOSE 8080, 9000) that return the
// incoming request (method, URL, headers, body) as JSON.
//
// Run with: npx tsx examples/http-server
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as hive from "../../src";
import { createShutdown } from "../shutdown.js";

const here = dirname(fileURLToPath(import.meta.url));
const imageTag = "http-server-image-bundle";

console.log(`> Building sandbox bundle ${imageTag}`);
await buildBundle(join(here, "image"), imageTag);

console.log("> Starting sandbox");
const sandbox = await hive.getOrCreateSandbox("hive-http-server-example", {
  image: imageTag,
});

const { shutdown } = createShutdown(sandbox);

// Give the server time to start inside the container.
await new Promise((r) => setTimeout(r, 2000));

// port 8080 — full echo server
await request(8080, "GET", "/hello?foo=bar");
await request(8080, "POST", "/echo", "hello from the sandbox client");

// port 9000 — second service
await request(9000, "GET", "/ping");

await shutdown();

async function request(
  port: number,
  method: string,
  path: string,
  body?: string,
): Promise<void> {
  const url = `${sandbox.proxyUrl(port)}${path}`;
  console.log(`\n> ${method} ${url}`);

  const res = await fetch(url, {
    method,
    body,
    headers: body ? { "content-type": "text/plain" } : undefined,
  });

  const text = await res.text();
  console.log(`< ${res.status} ${res.statusText}`);
  console.log(text);
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
