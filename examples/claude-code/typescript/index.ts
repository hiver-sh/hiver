import { getOrCreateSandbox } from "@hiver.sh/client";

const apiKey = process.env.ANTHROPIC_API_KEY;
if (!apiKey) {
  console.error("Set ANTHROPIC_API_KEY to run this example.");
  process.exit(1);
}

const sandbox = await getOrCreateSandbox("claude-code", {
  image: "claude",
  env: { ANTHROPIC_API_KEY: apiKey },
});

// `claude -p` runs a single prompt non-interactively and prints the result.
const result = await sandbox.exec(["claude", "-p", "Fix the bug in src/main.ts"], {
  cwd: "/workspace",
});
console.log(result.stdout);
