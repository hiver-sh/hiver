import { getOrCreateSandbox } from "@hiver.sh/client";

const apiKey = process.env.OPENAI_API_KEY;
if (!apiKey) {
  console.error("Set OPENAI_API_KEY to run this example.");
  process.exit(1);
}

const sandbox = await getOrCreateSandbox("codex", {
  image: "codex",
  env: { OPENAI_API_KEY: apiKey },
});

// `codex exec` runs a single prompt non-interactively and prints the result.
const result = await sandbox.exec(["codex", "exec", "Add tests for src/parser.ts"], {
  cwd: "/workspace",
});
console.log(result.stdout);
