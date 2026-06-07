import { randomBytes } from "node:crypto";
import { getOrCreateSandbox, SandboxConfig } from "@hiver.sh/client";
import { accent, bold, dim, red } from "../theme.js";
import { createLoader } from "../hive.js";
import { requireDocker } from "../docker.js";
import { imageExistsLocally, pullImage, isHiverBundle } from "../compose/images.js";
import { bundleImage, isDirectory } from "../bundle/bundle.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";

const DEFAULT_IMAGE = "hiversh/agent-cli";

const cmd = withGateway(subcommand("start", "Start a sandbox on the gateway."))
  .argument("[key]", "sandbox key (generated when omitted)")
  .option(
    "--image <image>",
    `agent image or Dockerfile directory to launch (default: ${DEFAULT_IMAGE})`,
  )
  .option("--entrypoint <entrypoint>", "override the image entrypoint")
  .option("--ttl <seconds>", "idle seconds before SIGTERM (0 disables)");
run(cmd);

const {
  gatewayUrl: gatewayFlag,
  image,
  entrypoint,
  ttl: ttlFlag,
} = cmd.opts();
const gatewayUrl = resolveGatewayUrl(gatewayFlag);
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

console.log();

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
if (entrypoint !== undefined) config.entrypoint = entrypoint;
if (ttl !== undefined) config.ttl = ttl;

// Cap the provision + readiness wait so a sandbox that never comes up (e.g. an
// image that exits on start) fails fast instead of hanging on the default 30s.
const START_TIMEOUT_MS = 5_000;

const loader = createLoader(`Starting ${dim(key)}`).start();
try {
  const sandbox = await getOrCreateSandbox(key, config, {
    gatewayUrl,
    timeoutMs: START_TIMEOUT_MS,
  });
  loader.succeed(`${accent(sandbox.key)}  ${dim(sandbox.id)}\n`);
} catch (err) {
  loader.fail(`could not start sandbox: ${dim(String(err))}`);
  // A common cause is an image whose entrypoint runs to completion: the
  // container exits right after starting, so the sandbox never comes up. Only
  // worth suggesting when the caller hasn't already set an entrypoint.
  if (entrypoint === undefined) {
    console.error(
      `\n${dim("This may be because the container exited right after starting.")}` +
        `\n${dim("Workaround: keep it alive by passing")} ${bold(`--entrypoint="tail -f /dev/null"`)}\n`,
    );
  } else {
    console.error();
  }
  process.exit(1);
}
