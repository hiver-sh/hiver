import { readFileSync } from "node:fs";
import { getOrCreateSandbox, SandboxConfig } from "@hiver.sh/client";

// The sandbox launch config: the image built by `npm run build` plus the egress
// policy that injects the API key at the proxy. Read from the same
// agent/.hiver.json the image was bundled from, so the key never lives in the
// sandbox.
const config = SandboxConfig.parse(
  JSON.parse(
    readFileSync(new URL("./agent/.hiver.json", import.meta.url), "utf8"),
  ),
);

// Provision (or reuse) the sandbox from the built image, then drive its
// in-sandbox server over the Hiver client.
const sandbox = await getOrCreateSandbox("openai-agents-sdk-ts", config);

const res = await fetch(`${sandbox.proxyUrl(3000)}chat`, {
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify({ prompt: "Create /workspace/fib.py and run it." }),
});

const { reply } = (await res.json()) as { reply: string };
console.log(reply);
