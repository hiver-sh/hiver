import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { brand, bright, bold, dim, red } from "../theme.js";

// src/compose (and dist/compose) → repo root, where docker/ lives.
const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, "../../../../..");
const COMPOSE_FILE = "docker/compose.yaml";

// Shared by `up` and `down`; the command name selects the action. Any extra
// args (e.g. `hiver up --build`) are forwarded to docker compose.
const action = process.argv[2] === "down" ? "down" : "up";
const extra = process.argv.slice(3);
const composeArgs =
  action === "up"
    ? ["compose", "-f", COMPOSE_FILE, "up", "-d", ...extra]
    : ["compose", "-f", COMPOSE_FILE, "down", ...extra];

const label = action === "up" ? "Starting the stack" : "Stopping the stack";
console.log(
  `\n${bold(brand(label))} ${dim(`docker ${composeArgs.join(" ")}`)}\n`,
);

const child = spawn("docker", composeArgs, {
  cwd: REPO_ROOT,
  stdio: "inherit",
});
child.on("error", (err) => {
  console.error(`\n  ${red("✖")} ${err.message}\n`);
  process.exit(1);
});
child.on("exit", (code) => {
  if (code === 0) {
    console.log(
      `\n  ${bright("✔")} stack ${action === "up" ? "up" : "down"}\n`,
    );
  }
  process.exit(code ?? 0);
});
