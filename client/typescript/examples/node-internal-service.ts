// Demonstrates internal service communication from inside the sandbox.
// Starts with a deny-all egress policy, fetches the controller (expected to
// fail), then uses applyConfig to allow the controller host and fetches again.
//
// Run with: npx tsx examples/node-internal-service.ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-node-internal-service", {
  image: "node",
  egress: [{ access: "deny", host: "*" }],
});

const script = `
// Internal port serves /v1/... directly; the /controller prefix is a
// gateway-routing concern and is NOT part of the controller's own API.
const res = await fetch("http://controller:9000/v1/sandboxes");
if (!res.ok) {
  console.error("HTTP", res.status, await res.text());
  process.exit(1);
}
const sandboxes = await res.json();
console.log(JSON.stringify(sandboxes, null, 2));
`;

await sandbox.writeFile("/workspace/list-sandboxes.mjs", script);

async function fetchSandboxes(label: string) {
  console.info(`\n--- ${label} ---`);
  const result = await sandbox.exec("node /workspace/list-sandboxes.mjs", {
    cwd: "/workspace",
  });
  if (result.stdout) console.info("stdout:\n" + result.stdout);
  if (result.stderr) console.error("stderr: " + result.stderr);
  console.info("exit code:", result.exit_code);
}

// First fetch: egress denies all hosts, expected to fail.
await fetchSandboxes("deny all — expect failure");

// Open up the controller host only, keep the catch-all deny after it.
const current = await sandbox.getConfig();
const result = await sandbox.applyConfig({
  ...current,
  egress: [
    { access: "allow", host: "controller" },
    { access: "deny", host: "*" },
  ],
});

if (!result.applied) {
  console.error("applyConfig rolled back:", result.error);
  process.exit(1);
}
console.info("\napplyConfig changes:", JSON.stringify(result.changes, null, 2));

await fetchSandboxes("allow controller — expect success");

await sandbox.shutdown();
