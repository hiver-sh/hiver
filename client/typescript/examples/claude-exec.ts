// Run Claude inside the sandbox and print the buffered result.
//
// Run with:
//   ANTHROPIC_API_KEY=sk-ant-... \
//     npx tsx examples/claude-exec.ts
import process from "node:process";

import * as hiver from "@hiver.sh/client";

if (!process.env.ANTHROPIC_API_KEY) {
  console.error("ANTHROPIC_API_KEY must be defined");
  process.exit(1);
}

const sandbox = await hiver.getOrCreateSandbox("hive-claude-exec", {
  image: "hiversh/agent-cli:latest",
  env: { ANTHROPIC_API_KEY: process.env.ANTHROPIC_API_KEY },
});

const result = await sandbox.exec(
  "claude -p 'Write a poem and save it as pdf'",
  {
    cwd: "/workspace",
  },
);
console.log(result.stdout);

if (result.stderr) console.error(result.stderr);
