import { randomBytes } from "node:crypto";
import { getOrCreateSandbox, SandboxConfig } from "@hiver.sh/client";
import { white, bold, dim, red, accent } from "../theme.js";
import { createLoader } from "../hive.js";
import { requireDocker } from "../docker.js";
import {
  imageExistsLocally,
  pullImage,
  isHiverBundle,
} from "../compose/images.js";
import { bundleImage, isDirectory } from "../bundle/bundle.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { ensureGateway } from "../gateway.js";
import {
  selectAgentEntrypoint,
  applyAgentCliDefaults,
} from "./agent-cli.js";
import { sandboxImages } from "../container-config.js";

const DEFAULT_IMAGE = sandboxImages["agent-cli"] ?? "hiversh/agent-cli:latest";

const cmd = withGateway(subcommand("start", "Start a sandbox on the gateway."))
  .argument("[key]", "sandbox key (generated when omitted)")
  .option(
    "--image <image>",
    `agent image or Dockerfile directory to launch (default: ${DEFAULT_IMAGE})`,
  )
  .option("--entrypoint <entrypoint>", "override the image entrypoint")
  .option("--ttl <seconds>", "idle seconds before SIGTERM (0 disables)")
  .option("--tty", "attach a pseudo-TTY to the entrypoint");
run(cmd);

const { gatewayUrl: gatewayFlag, image, entrypoint, ttl: ttlFlag, tty } = cmd.opts();
let gatewayUrl = resolveGatewayUrl(gatewayFlag);
const key = cmd.args[0] ?? `agent-${randomBytes(2).toString("hex")}`;

// Validate `--ttl` up front (cheap, no Docker/network) so a bad value fails
// before we start pulling or bundling. It arrives as a string from commander.
let ttl: number | undefined;
if (ttlFlag !== undefined) {
  ttl = Number(ttlFlag);
  if (!Number.isInteger(ttl) || ttl < 0) {
    console.error(
      `\n  ${red("✖")} --ttl must be a non-negative integer, got ${bold(String(ttlFlag))}\n`,
    );
    process.exit(1);
  }
}

const imageArg = image ?? DEFAULT_IMAGE;

let resolvedEntrypoint: string | undefined = entrypoint;
if (imageArg === DEFAULT_IMAGE && entrypoint === undefined) {
  resolvedEntrypoint = await selectAgentEntrypoint();
}

console.log();

// Make sure the stack is up before doing any (slow) image work, so a down
// gateway fails fast with the offer to start it rather than after bundling.
gatewayUrl = await ensureGateway(gatewayUrl);

// A sandbox runtime image must be a Hiver bundle (it boots `sandboxd`, which
// reads the unpacked agent tar under /mnt). Resolve the requested image to one:
// a directory is built + bundled; an image ref is used as-is when it already is
// a bundle, otherwise it's bundled automatically.
await requireDocker();
let resolvedImage: string;
try {
  if (isDirectory(imageArg)) {
    resolvedImage = await bundleImage(imageArg);
  } else {
    // Need it locally to inspect for the bundle markers; pull if absent.
    if (!imageExistsLocally(imageArg)) {
      const pull = createLoader(`Pulling ${imageArg}`).start();
      const { ok, output } = await pullImage(imageArg);
      if (!ok) {
        pull.fail(`could not pull ${imageArg}`);
        if (output.trim()) process.stderr.write("\n" + output.trimEnd() + "\n");
        process.exit(1);
      }
      pull.succeed(`Pulled ${imageArg}`);
    }
    resolvedImage = isHiverBundle(imageArg)
      ? imageArg
      : await bundleImage(imageArg);
  }
} catch (err) {
  console.error(
    `  ${red("✖")} ${err instanceof Error ? err.message : String(err)}\n`,
  );
  process.exit(1);
}

// Only forward flags the caller actually set, so the controller's defaults
// apply otherwise.
const config: SandboxConfig = { image: resolvedImage };
if (resolvedEntrypoint !== undefined) config.entrypoint = resolvedEntrypoint;
if (ttl !== undefined) config.ttl = ttl;
if (tty) config.tty = true;
if (imageArg === DEFAULT_IMAGE) applyAgentCliDefaults(config);

// Cap the provision + readiness wait so a sandbox that never comes up (e.g. an
// image that exits on start) fails fast instead of hanging on the default 30s.
const START_TIMEOUT_MS = 5_000;

const loader = createLoader(`Starting ${dim(key)}`).start();
try {
  const sandbox = await getOrCreateSandbox(key, config, {
    gatewayUrl,
    timeoutMs: START_TIMEOUT_MS,
  });
  // Show the exposed ports (like `hiver list`) rather than the opaque id;
  // tolerate a slow/failed lookup so a started sandbox still reports success.
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
  // A common cause is an image whose entrypoint runs to completion: the
  // container exits right after starting, so the sandbox never comes up. Only
  // worth suggesting when the caller hasn't already set an entrypoint.
  if (resolvedEntrypoint === undefined) {
    console.error(
      `\n${dim("This may be because the container exited right after starting.")}` +
        `\n${dim("Workaround: keep it alive by passing")} ${bold(`--entrypoint="tail -f /dev/null"`)}\n`,
    );
  } else {
    console.error();
  }
  process.exit(1);
}
