import { spawn } from "node:child_process";
import { dirname } from "node:path";
import { DEFAULT_GATEWAY_URL } from "@hiver.sh/client";
import { createLoader } from "../hive.js";
import { requireDocker } from "../docker.js";
import { dim, bright } from "../theme.js";
import { writeConfig } from "../config.js";
import { subcommand, run } from "../args.js";
import { missingImages, pullImage } from "./images.js";
import { findAvailablePort } from "./port.js";
import {
  runningContainers,
  publishedPort,
  removeNamespaceContainers,
} from "./stack.js";
import { composePath, sandboxImages, sandboxImagesMicrovm } from "../container-config.js";

// Shared by `up` and `down`; the command name selects the action. Unknown args
// (e.g. `hiver up --build`) are forwarded to docker compose.
const action = process.argv[2] === "down" ? "down" : "up";
const cli = subcommand(
  action,
  action === "up" ? "Start the stack." : "Stop the stack.",
)
  .allowUnknownOption()
  .allowExcessArguments()
  .addHelpText(
    "after",
    "\nAny extra arguments are forwarded to docker compose.",
  );
if (action === "up") {
  cli.option(
    "--no-pack",
    "Disable packing mode: give each sandbox its own container instead of packing N same-image sandboxes into one.",
  );
  cli.option("--microvm", "Use microVM image variants instead of container images (requires KVM).");
}
run(cli); // exits here on --help
// Packing is the default; `--no-pack` opts out (Commander sets pack=false).
const pack = action === "up" && cli.opts().pack !== false;
const microvm = action === "up" && !!cli.opts().microvm;
// --no-pack and --microvm are ours, not docker compose flags — don't forward them.
const extra = process.argv.slice(3).filter((a) => a !== "--no-pack" && a !== "--microvm");

// Parsed (so `--help` works without Docker) — now require Docker.
await requireDocker();

const composeFile = composePath;

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
    const port =
      publishedPort(composeFile, "gateway", DEFAULT_PORT) ?? DEFAULT_PORT;
    writeConfig({ gatewayUrl: gatewayUrl(port) });
    console.log(`\n${bright("✔")} Local stack already running`);
    console.log(`  ${dim("gateway")} → ${bright(gatewayUrl(port))}\n`);
    process.exit(0);
  }

  // Pick a free host port for the gateway if the default is taken.
  gatewayPort = await findAvailablePort(DEFAULT_PORT);
  env.GATEWAY_PORT = String(gatewayPort);

  // Build the images config (logical name → ref) straight from the bundled
  // catalog (sandbox-images.json) and inject it into the controller as inline
  // JSON (HIVER_IMAGES_CONFIG). Reading the catalog directly means new bundled
  // images (e.g. a new default) always reach the controller without depending on
  // a previously-seeded ~/.hiver/config.json. Packing: --no-pack sets the
  // file-wide pack default to false; otherwise the controller's default (true)
  // applies.
  const catalog = microvm ? sandboxImagesMicrovm : sandboxImages;
  const images: Record<string, { ref: string }> = {};
  for (const [name, ref] of Object.entries(catalog)) images[name] = { ref };
  const imagesEnv: { pack?: boolean; images: typeof images } = { images };
  if (!pack) imagesEnv.pack = false;
  env.HIVER_IMAGES_CONFIG = JSON.stringify(imagesEnv);

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

// Controller-spawned sandbox containers carry the `hiver` project label but
// aren't compose services, so `down` must force-remove them itself — otherwise
// they linger and keep the stack network from being torn down.
const removedSandboxes = action === "down" ? removeNamespaceContainers() : 0;

const composeArgs =
  action === "up"
    ? ["compose", "-f", composeFile, "up", "-d", ...extra]
    : ["compose", "-f", composeFile, "down", ...extra];

// The hive loader runs while docker works; its output is captured and only
// surfaced if the command fails.
console.log();
const loader = createLoader(
  action === "up" ? "Starting the stack" : "Stopping the stack",
).start();

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
    if (action === "up") {
      loader.succeed("Local stack up");
      // Persist the variant so `start` resolves logical image names against the
      // matching catalog (container vs microvm).
      writeConfig({ gatewayUrl: gatewayUrl(gatewayPort), microvm });
      console.log(`  ${dim("gateway")} → ${bright(gatewayUrl(gatewayPort))}\n`);
    } else {
      loader.succeed(
        removedSandboxes > 0
          ? `Local stack down ${dim(`(removed ${removedSandboxes} sandbox container${removedSandboxes === 1 ? "" : "s"})`)}\n`
          : `Local stack down\n`,
      );
    }
  } else {
    loader.fail(`docker compose ${action} failed (exit ${code})`);
    if (output.trim()) process.stderr.write("\n" + output.trimEnd() + "\n");
  }
  process.exit(code ?? 0);
});
