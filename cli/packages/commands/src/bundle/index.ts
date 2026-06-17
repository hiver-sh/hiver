import { red } from "../theme.js";
import { requireDocker } from "../docker.js";
import { bundleImage } from "./bundle.js";
import { subcommand, run as parseCli } from "../args.js";

const cli = subcommand(
  "bundle",
  "Bundle a Docker image into a Hiver runtime image.",
)
  .argument("<image>", "Docker image or directory with a Dockerfile to bundle")
  .option("--tag <tag>", "runtime image tag (default: <image>-bundled)")
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

// Parse first (so `--help` works without Docker), then require Docker.
await requireDocker();

try {
  await bundleImage(arg, {
    tag: opts.tag,
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
