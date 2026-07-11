import { randomBytes } from "node:crypto";
import { z } from "zod";
import { getOrCreateSandbox, SandboxConfig } from "@hiver.sh/client";
import { white, bold, dim, red, accent } from "../theme.js";
import { createLoader } from "../hive.js";
import { requireDocker } from "../docker.js";
import { isHiverBundle } from "../compose/images.js";
import { bundleImage, isDirectory } from "../bundle/bundle.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { ensureGateway, isLocalGateway } from "../gateway.js";
import { readConfig } from "../config.js";
import { resolveSandboxImage, imageConfig } from "../container-config.js";
import { selectImage } from "./agent-cli.js";

const DEFAULT_IMAGE = "agent-base";

// Read a JSON `SandboxConfig` piped to stdin, for anything the flags don't cover
// (most notably `env`). An interactive TTY carries no config, so it's skipped;
// an empty pipe is treated as no config. A malformed body is a hard error so a
// typo'd config can't be silently ignored.
async function readStdinConfig(): Promise<SandboxConfig> {
  if (process.stdin.isTTY) return {};
  process.stdin.setEncoding("utf8");
  let raw = "";
  for await (const chunk of process.stdin) raw += chunk;
  if (raw.trim() === "") return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    console.error(
      `\n  ${red("✖")} could not parse stdin as JSON: ${dim(err instanceof Error ? err.message : String(err))}\n`,
    );
    process.exit(1);
  }
  const result = SandboxConfig.safeParse(parsed);
  if (!result.success) {
    console.error(
      `\n  ${red("✖")} invalid sandbox config from stdin:\n\n${z.prettifyError(result.error)}\n`,
    );
    process.exit(1);
  }
  return result.data;
}

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

const {
  gatewayUrl: gatewayFlag,
  image,
  entrypoint,
  ttl: ttlFlag,
  tty,
} = cmd.opts();
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

// Read any piped config before resolving the image: stdin can supply the image
// (as in the docs' `env` example) and provides fields flags don't cover.
const stdinConfig = await readStdinConfig();

// With no image from `--image`, stdin, or `--entrypoint`, let the user pick one
// to launch; otherwise honour the flag, then stdin, falling back to the base
// image. `--image` wins over stdin's `image` when both are set.
const pickImage =
  image === undefined &&
  entrypoint === undefined &&
  stdinConfig.image === undefined;
const imageArg = pickImage
  ? await selectImage()
  : (image ?? stdinConfig.image ?? DEFAULT_IMAGE);

const resolvedEntrypoint: string | undefined = entrypoint;

console.log();

// Make sure the stack is up before doing any (slow) image work, so a down
// gateway fails fast with the offer to start it rather than after bundling.
gatewayUrl = await ensureGateway(gatewayUrl);

// A sandbox runtime image must be a Hiver bundle (it boots `sandboxd`, which
// reads the unpacked agent tar under /mnt). For a local gateway — which shares
// this host's docker daemon — the CLI resolves the requested image to a bundle:
// a directory is built + bundled; an image ref is used as-is when it already is
// a bundle, otherwise it's bundled automatically. A remote gateway pulls images
// itself, so the CLI touches no local docker and hands the image through as-is
// (a logical catalog name is resolved by the remote controller's own config).
let resolvedImage: string;
if (!isLocalGateway(gatewayUrl)) {
  if (isDirectory(imageArg)) {
    console.error(
      `\n  ${red("✖")} cannot build a local Dockerfile directory (${imageArg}) for a remote gateway ${dim(gatewayUrl)}\n`,
    );
    process.exit(1);
  }
  resolvedImage = imageArg;
} else {
  await requireDocker();
  try {
    if (isDirectory(imageArg)) {
      resolvedImage = await bundleImage(imageArg);
    } else {
      // A logical catalog name (e.g. `browser`) resolves to its concrete ref from
      // sandbox-images.json, matching the variant the local stack was brought up
      // with (container vs microvm). Raw refs and unknown names pass through.
      const ref =
        resolveSandboxImage(imageArg, Boolean(readConfig().microvm)) ??
        imageArg;
      // A ref that's already a bundle (a local build, or a catalog image the
      // stack pulled at `up`) is used as-is; anything else is bundled, which
      // pulls the source image first when it isn't already local. Getting the
      // final runtime image onto the host for `docker create` is the
      // controller's job (see the docker runtime's ensureImage), so this path
      // no longer pre-pulls before starting.
      resolvedImage = isHiverBundle(ref) ? ref : await bundleImage(ref);
    }
  } catch (err) {
    console.error(
      `  ${red("✖")} ${err instanceof Error ? err.message : String(err)}\n`,
    );
    process.exit(1);
  }
}

// Start from the logical image's launch defaults from sandbox-images.json (e.g.
// tty/cwd for the agent images); empty for raw refs / Dockerfile dirs. The
// stdin config, resolved ref, and any flags the caller set are layered on top,
// so explicit flags win over stdin which wins over the image defaults, and the
// controller's defaults apply to anything left unset.
const config: SandboxConfig = {
  ...imageConfig(imageArg),
  ...stdinConfig,
  image: resolvedImage,
};
// entrypoint accepts a string (the sandbox splits it on whitespace) or an
// argv array; the flag and agent picker both yield a string, so pass it as-is.
if (resolvedEntrypoint !== undefined) config.entrypoint = resolvedEntrypoint;
if (ttl !== undefined) config.ttl = ttl;
if (tty) config.tty = true;

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
