
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { DEFAULT_GATEWAY_URL } from "@hiver.sh/client";
import { createLoader } from "../hive.js";
import { requireDocker } from "../docker.js";
import { dim, bright } from "../theme.js";
import { writeConfig } from "../config.js";
import { subcommand, run } from "../args.js";
import { missingImages, pullImage } from "./images.js";
import { findAvailablePort } from "./port.js";
import { runningContainers, publishedPort } from "./stack.js";

// Shared by `up` and `down`; the command name selects the action. Unknown args
// (e.g. `hiver up --build`) are forwarded to docker compose.
const action = process.argv[2] === "down" ? "down" : "up";
const cli = subcommand(action, action === "up" ? "Start the stack." : "Stop the stack.")
  .allowUnknownOption()
  .allowExcessArguments()
  .addHelpText("after", "\nAny extra arguments are forwarded to docker compose.");
run(cli); // exits here on --help
const extra = process.argv.slice(3);

// Parsed (so `--help` works without Docker) — now require Docker.
await requireDocker();

// An optimized compose lives next to this module (copied into dist/ at build
// time), so it ships with the CLI rather than reaching into the monorepo.
const __dirname = dirname(fileURLToPath(import.meta.url));
const composeFile = resolve(__dirname, "compose.yaml");

// Default gateway port (the container always listens on this); the published
// host port may differ when the default is taken.
const DEFAULT_PORT = Number(new URL(DEFAULT_GATEWAY_URL).port) || 10000;
const gatewayUrl = (port: number) => {
  const u = new URL(DEFAULT_GATEWAY_URL);
  u.port = String(port);
  return u.origin;
};

const env: NodeJS.ProcessEnv = { ...process.env };
let gatewayPort = DEFAULT_PORT;

if (action === "up") {
  // Already running? Report it (with its actual port) and stop.
  if (runningContainers(composeFile).length > 0) {
    const port = publishedPort(composeFile, "gateway", DEFAULT_PORT) ?? DEFAULT_PORT;
    writeConfig({ gatewayUrl: gatewayUrl(port) });
    console.log(`\n${bright("✔")} Local stack already running`);
    console.log(`  ${dim("gateway")} → ${bright(gatewayUrl(port))}\n`);
    process.exit(0);
  }

  // Pick a free host port for the gateway if the default is taken.
  gatewayPort = await findAvailablePort(DEFAULT_PORT);
  env.GATEWAY_PORT = String(gatewayPort);

  // The stack images must be present locally; pull any that are missing.
  for (const image of missingImages(composeFile)) {
    const pull = createLoader(`Pulling ${image}`).start();
    const { ok, output } = await pullImage(image);
    if (!ok) {
      pull.fail(`could not pull ${image}`);
      if (output.trim()) process.stderr.write("\n" + output.trimEnd() + "\n");
      process.exit(1);
    }
    pull.succeed(`Pulled ${image}`);
  }
}

const composeArgs =
  action === "up"
    ? ["compose", "-f", composeFile, "up", "-d", ...extra]
    : ["compose", "-f", composeFile, "down", ...extra];

// The hive loader runs while docker works; its output is captured and only
// surfaced if the command fails.
console.log();
const loader = createLoader(action === "up" ? "Starting the stack" : "Stopping the stack").start();

let output = "";
const child = spawn("docker", composeArgs, {
  cwd: dirname(composeFile),
  env,
  stdio: ["ignore", "pipe", "pipe"],
});
child.stdout?.on("data", (d: Buffer) => (output += d));
child.stderr?.on("data", (d: Buffer) => (output += d));

process.on("SIGINT", () => {
  loader.stop();
  child.kill("SIGINT");
});

child.on("error", (err) => {
  loader.fail(err.message);
  process.exit(1);
});
child.on("exit", (code) => {
  if (code === 0) {
    loader.succeed(`Local stack ${action === "up" ? "up" : "down"}`);
    if (action === "up") {
      writeConfig({ gatewayUrl: gatewayUrl(gatewayPort) });
      console.log(`  ${dim("gateway")} → ${bright(gatewayUrl(gatewayPort))}\n`);
    }
  } else {
    loader.fail(`docker compose ${action} failed (exit ${code})`);
    if (output.trim()) process.stderr.write("\n" + output.trimEnd() + "\n");
  }
  process.exit(code ?? 0);
});

