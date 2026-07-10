import { readFileSync } from "node:fs";
import { getOrCreateSandbox, SandboxConfig } from "@hiver.sh/client";

// The agent authenticates with your OpenAI API key, read from the environment.
// Running without it exits with an error.
const apiKey = process.env.OPENAI_API_KEY;
if (!apiKey) {
  console.error("Set OPENAI_API_KEY to run this example.");
  process.exit(1);
}

// Launch config from agent/.hiver.json (image + egress policy). Inject the key
// into the egress override so it's applied at the proxy on the way to
// api.openai.com — never written into the sandbox itself. The .hiver.json keeps
// only a placeholder.
const config = SandboxConfig.parse(
  JSON.parse(
    readFileSync(new URL("./agent/.hiver.json", import.meta.url), "utf8"),
  ),
);
const headers = config.egress?.find((rule) => rule.host === "api.openai.com")
  ?.override?.headers;
if (headers) headers["Authorization"] = `Bearer ${apiKey}`;

// Provision (or reuse) the sandbox from the built image, then drive its
// in-sandbox server over the Hiver client.
const sandbox = await getOrCreateSandbox("openai-agents-sdk-ts", config);

const res = await fetch(`${sandbox.proxyUrl(3000)}chat`, {
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify({ prompt: "Create /workspace/fib.py and run it." }),
});
if (!res.ok) {
  console.error(`agent server returned ${res.status}: ${await res.text()}`);
  process.exit(1);
}

const { reply } = (await res.json()) as { reply: string };
console.log(reply);
