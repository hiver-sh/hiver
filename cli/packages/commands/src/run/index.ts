import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join, resolve } from "node:path";
import { z } from "zod";
import {
  getOrCreateSandbox,
  listSandboxes,
  SandboxConfig,
} from "@hiver.sh/client";
import { white, bold, dim, red, accent, brand } from "../theme.js";
import { createLoader } from "../hive.js";
import { requireDocker } from "../docker.js";
import { bundleImage, isDirectory } from "../bundle/bundle.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { ensureGateway, isLocalGateway } from "../gateway.js";
import { confirm } from "../prompt.js";

// The per-project config `hiver run` reads and writes. It records the sandbox
// key and the `SandboxConfig` used to launch the project, so a second `hiver
// run` reuses the same setup (only the image is re-bundled from the directory).
const HIVER_JSON = ".hiver.json";

// Keep-alive entrypoint for images whose command would otherwise exit right
// away, so the sandbox stays up until it's stopped or times out.
const KEEP_ALIVE = 'ENTRYPOINT ["tail", "-f", "/dev/null"]';

// A minimal Dockerfile scaffolded when the directory has none. `tail -f
// /dev/null` keeps the container (and so the sandbox) alive indefinitely.
const DEFAULT_DOCKERFILE = `FROM ubuntu:24.04

# Keep the sandbox alive so you can shell in and run commands.
${KEEP_ALIVE}
`;

function fail(message: string): never {
  console.error(`\n  ${red("✖")} ${message}\n`);
  process.exit(1);
}

// Read the `SandboxConfig` stored in `.hiver.json`, or return undefined when
// absent. A malformed file is a hard error so a typo can't be silently ignored
// and overwritten.
function readProjectConfig(path: string): SandboxConfig | undefined {
  if (!existsSync(path)) return undefined;
  let parsed: unknown;
  try {
    parsed = JSON.parse(readFileSync(path, "utf8"));
  } catch (err) {
    fail(
      `could not parse ${HIVER_JSON}: ${dim(err instanceof Error ? err.message : String(err))}`,
    );
  }
  const result = SandboxConfig.safeParse(parsed);
  if (!result.success) {
    fail(`invalid ${HIVER_JSON}:\n\n${z.prettifyError(result.error)}`);
  }
  return result.data;
}

const cmd = withGateway(
  subcommand("run", "Build and launch a project directory as a sandbox."),
)
  .argument("[dir]", "project directory (default: current directory)")
  .argument("[key]", "sandbox key (default: directory name)");
run(cmd);

const gatewayFlag = cmd.opts().gatewayUrl as string | undefined;
let gatewayUrl = resolveGatewayUrl(gatewayFlag);

const dir = resolve(cmd.args[0] ?? ".");
if (!isDirectory(dir)) {
  fail(`not a directory: ${white(dir)}`);
}

// The directory name is the bundle tag (and the default sandbox key). Sanitize
// it to a valid Docker tag the same way `bundleImage` does for a build context.
const name = basename(dir)
  .toLowerCase()
  .replace(/[^a-z0-9-]/g, "-");

const configPath = join(dir, HIVER_JSON);
const projectConfig = readProjectConfig(configPath);
// The sandbox key defaults to the directory name, overridable as the second arg.
const key = cmd.args[1] ?? name;

// Ensure the directory has a Dockerfile. Without one we can't build an image;
// offer to scaffold a keep-alive default rather than failing outright.
const dockerfilePath = join(dir, "Dockerfile");
if (!existsSync(dockerfilePath)) {
  console.log(
    `\n  ${dim(`No Dockerfile in`)} ${white(dir)}${dim(".")}`,
  );
  const create = await confirm(
    `  Create one that keeps the sandbox alive with ${bold("tail -f /dev/null")}?`,
  );
  if (!create) {
    fail(`a Dockerfile is required to run ${white(name)}`);
  }
  writeFileSync(dockerfilePath, DEFAULT_DOCKERFILE);
  console.log(`  ${brand("✔")} wrote ${white(dockerfilePath)}`);
}

console.log();

// Building the directory needs a local docker daemon, so `run` only works
// against a local gateway (a remote one pulls images itself and can't see this
// host's build context).
gatewayUrl = await ensureGateway(gatewayUrl);
if (!isLocalGateway(gatewayUrl)) {
  fail(
    `cannot build a local directory for a remote gateway ${dim(gatewayUrl)}`,
  );
}

// A sandbox already running under this key won't pick up the freshly-built
// image — `getOrCreateSandbox` would just hand back the running one. Ask before
// tearing it down, and bail (rather than build) if the user declines.
const existing = (await listSandboxes({ gatewayUrl }).catch(() => [])).find(
  (s) => s.key === key,
);
if (existing) {
  const ok = await confirm(
    `  Sandbox ${white(key)} is already running. Stop it and relaunch?`,
  );
  if (!ok) fail(`left ${white(key)} running`);
  const stopping = createLoader(`Stopping ${dim(key)}`).start();
  try {
    await existing.shutdown();
    // DELETE only cancels the lifecycle — teardown is async, so wait until the
    // key really disappears before relaunching, otherwise the new sandbox would
    // collide with the one still shutting down.
    const deadline = Date.now() + 30_000;
    let gone = false;
    while (Date.now() < deadline) {
      const running = (await listSandboxes({ gatewayUrl }).catch(() => [])).some(
        (s) => s.key === key,
      );
      if (!running) {
        gone = true;
        break;
      }
      await new Promise((r) => setTimeout(r, 250));
    }
    if (!gone) {
      stopping.fail(`timed out waiting for ${white(key)} to stop`);
      process.exit(1);
    }
    stopping.succeed(`${white(key)} ${dim("stopped")}`);
  } catch (err) {
    stopping.fail(`could not stop ${white(key)}: ${dim(String(err))}`);
    process.exit(1);
  }
}

await requireDocker();

// The bundle tag comes from `.hiver.json`'s `image` when set, so editing it
// renames the image; otherwise it defaults to the directory name.
const imageTag = projectConfig?.image ?? name;
let resolvedImage: string;
try {
  resolvedImage = await bundleImage(dir, { tag: imageTag });
} catch (err) {
  fail(err instanceof Error ? err.message : String(err));
}

const config: SandboxConfig = { ...projectConfig, image: resolvedImage };

// Persist the config so a later `hiver run` reuses the same settings. Announce
// it only on first creation — a re-run silently refreshes the existing file.
const created = projectConfig === undefined;
writeFileSync(configPath, JSON.stringify(config, null, 2) + "\n");
if (created) {
  console.log(`  ${brand("✔")} wrote ${white(configPath)}`);
}

const loader = createLoader(`Starting ${dim(key)}`).start();
try {
  // Generous enough for the controller to pull the runtime image on first use;
  // the CLI never pulls itself. Success still returns as soon as it's ready.
  const sandbox = await getOrCreateSandbox(key, config, {
    gatewayUrl,
    timeoutMs: 300_000,
  });
  const ports = await sandbox.getPorts({ timeoutMs: 5_000 }).catch(() => []);
  const exposed = ports.length
    ? `  ${dim(ports.map((p) => `:${p}`).join(", "))}`
    : "";
  loader.succeed(`${white(sandbox.key)} ${dim("started")}${exposed}`);
  console.log(
    `\n  ${accent(`hiver shell ${sandbox.key}`)}    ${dim("open a shell")}` +
      `\n  ${accent(`hiver inspect ${sandbox.key}`)}  ${dim("inspect this agent")}` +
      `\n  ${accent(`hiver events ${sandbox.key}`)}   ${dim("stream events")}` +
      `\n  ${accent(`hiver stop ${sandbox.key}`)}     ${dim("stop sandbox")}\n`,
  );
  process.exit(0);
} catch (err) {
  loader.fail(`could not start sandbox: ${dim(String(err))}`);
  console.error();
  process.exit(1);
}
