import { getOrCreateSandbox } from "@hiver.sh/client";

const token = process.env.GITHUB_TOKEN;
if (!token) {
  console.error("Set GITHUB_TOKEN to run this example.");
  process.exit(1);
}

const sandbox = await getOrCreateSandbox("copilot", {
  image: "copilot",
  env: { GITHUB_TOKEN: token },
});

// `copilot -p` runs a single prompt non-interactively and prints the result.
const result = await sandbox.exec(
  ["copilot", "-p", "Explain what src/server.ts does", "--allow-all-tools"],
  { cwd: "/workspace" },
);
console.log(result.stdout);
