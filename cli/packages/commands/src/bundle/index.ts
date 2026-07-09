import { existsSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { red } from "../theme.js";
import { requireDocker } from "../docker.js";
import { bundleImage, isDirectory } from "./bundle.js";
import { subcommand, run as parseCli } from "../args.js";

// The per-project config `hiver run` reads and writes; `bundle` reuses its
// `image` field as the default tag so both commands bundle to the same name.
const HIVER_JSON = ".hiver.json";

const cli = subcommand(
  "bundle",
  "Bundle a Docker image into a Hiver runtime image.",
)
  .argument("<image>", "Docker image or directory with a Dockerfile to bundle")
  .option(
    "--tag <tag>",
    "runtime image tag (default: image from .hiver.json, else <image>-bundled)",
  )
  .option(
    "--entrypoint <command>",
    'override the source image entrypoint, e.g. --entrypoint="tail -f /dev/null"',
  )
  .option("--microvm", "microvm isolation")
  .option(
    "--push",
    "push the bundle to the registry",
  )
  .option(
    "--platform <platforms>",
    "comma-separated target platforms, e.g. linux/amd64,linux/arm64",
  );
parseCli(cli);

const arg = cli.args[0];
const opts = cli.opts();

const platforms = opts.platform
  ? String(opts.platform)
      .split(",")
      .map((p) => p.trim())
      .filter(Boolean)
  : undefined;

// Resolve the output tag. An explicit --tag wins; otherwise, when bundling a
// directory that has a .hiver.json, reuse its `image` field — the same tag
// `hiver run` bundles to — so `hiver bundle ./agent` needs no --tag and stays
// in sync. Falls back to bundleImage's `<image>-bundled` default when neither
// is set.
let tag = opts.tag ? String(opts.tag) : undefined;
if (!tag && arg && isDirectory(arg)) {
  const configPath = join(resolve(arg), HIVER_JSON);
  if (existsSync(configPath)) {
    let image: unknown;
    try {
      image = (
        JSON.parse(readFileSync(configPath, "utf8")) as { image?: unknown }
      ).image;
    } catch (err) {
      console.error(
        `\n  ${red("✖")} could not parse ${configPath}: ${err instanceof Error ? err.message : String(err)}\n`,
      );
      process.exit(1);
    }
    if (typeof image === "string" && image.length > 0) tag = image;
  }
}

// Parse first (so `--help` works without Docker), then require Docker.
await requireDocker();

try {
  await bundleImage(arg, {
    tag,
    entrypoint: opts.entrypoint ? String(opts.entrypoint) : undefined,
    microvm: Boolean(opts.microvm),
    push: Boolean(opts.push),
    platforms,
  });
} catch (err) {
  console.error(
    `\n  ${red("✖")} ${err instanceof Error ? err.message : String(err)}\n`,
  );
  process.exit(1);
}
