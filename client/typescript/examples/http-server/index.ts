// Demonstrates proxying HTTP requests to a service running inside the sandbox.
// The sandbox image runs two echo servers (EXPOSE 8080, 9000) that return the
// incoming request (method, URL, headers, body) as JSON.
//
// Run with: npx tsx examples/http-server
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as hiver from "@hiver.sh/client";
import { buildBundle, createShutdown } from "../utils/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const imageTag = "http-server-image-bundle";

console.log(`> Building sandbox bundle ${imageTag}`);
await buildBundle(join(here, "image"), imageTag);

console.log("> Starting sandbox");
const sandbox = await hiver.getOrCreateSandbox("hive-http-server-example", {
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
  // proxyUrl already ends with a slash, so drop any leading slash on the path.
  const url = `${sandbox.proxyUrl(port)}${path.replace(/^\//, "")}`;
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
