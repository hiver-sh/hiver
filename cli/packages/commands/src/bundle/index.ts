import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { basename, dirname, resolve, join } from "node:path";
import { brand, accent, bright, bold, dim, red } from "../theme.js";
import { createLoader } from "../hive.js";
import { requireDocker } from "../docker.js";
import { imageExistsLocally, pullImage } from "../compose/images.js";
import { subcommand, run as parseCli } from "../args.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const DOCKERFILE = resolve(__dirname, "bundler.Dockerfile");

const cli = subcommand(
  "bundle",
  "Bundle a Docker image into a Hiver runtime image.",
)
  .argument("<image>", "Docker image or directory with a Dockerfile to bundle")
  .option("--tag <tag>", "runtime image tag (default: <image>-bundled)")
  .option("--microvm", "microvm isolation");
parseCli(cli);

const arg = cli.args[0];
const opts = cli.opts();

// If the argument is a directory, build a Docker image from it first.
const resolvedArg = resolve(arg);
const isDir = existsSync(resolvedArg) && statSync(resolvedArg).isDirectory();
if (isDir && !existsSync(join(resolvedArg, "Dockerfile"))) {
  console.error(`\n  ${red("✖")} No Dockerfile found in ${resolvedArg}\n`);
  process.exit(1);
}
const image = isDir
  ? basename(resolvedArg)
      .toLowerCase()
      .replace(/[^a-z0-9-]/g, "-")
  : arg;

const tag = opts.tag ?? `${image.split(":")[0]}-bundled`;
const microvm: boolean = Boolean(opts.microvm);

// Parse first (so `--help` works without Docker), then require Docker.
await requireDocker();

if (isDir) {
  console.log(
    `\n${bold(brand("Build"))} ${accent(resolvedArg)} ${dim("→")} ${bright(image)}\n`,
  );
  await run("docker", ["build", "-t", image, resolvedArg], "inherit");
  console.log();
} else if (!imageExistsLocally(image)) {
  const pull = createLoader(`Pulling ${image}`).start();
  const { ok, output } = await pullImage(image);
  if (!ok) {
    pull.fail(`could not pull ${image}`);
    if (output.trim()) process.stderr.write("\n" + output.trimEnd() + "\n");
    process.exit(1);
  }
  pull.succeed(`Pulled ${image}`);
}

const BASE_IMAGE = "hiversh/core";
if (!imageExistsLocally(BASE_IMAGE)) {
  const pull = createLoader(`Pulling ${BASE_IMAGE}`).start();
  const { ok, output } = await pullImage(BASE_IMAGE);
  if (!ok) {
    pull.fail(`could not pull ${BASE_IMAGE}`);
    if (output.trim()) process.stderr.write("\n" + output.trimEnd() + "\n");
    process.exit(1);
  }
  pull.succeed(`Pulled ${BASE_IMAGE}`);
}

// Run a command; reject on a non-zero exit.
function run(
  cmd: string,
  cmdArgs: string[],
  stdio: "ignore" | "inherit",
): Promise<void> {
  return new Promise((res, rej) => {
    const child = spawn(cmd, cmdArgs, { stdio });
    child.on("error", rej);
    child.on("exit", (code) =>
      code === 0 ? res() : rej(new Error(`${cmd} exited with code ${code}`)),
    );
  });
}

console.log(
  `\n${bold(brand("Bundle"))} ${accent(image)} ${dim("→")} ${bright(tag)}\n`,
);

const ctx = mkdtempSync(join(tmpdir(), "hiver-bundle-"));
try {
  const loader = createLoader(`saving ${image}`).start();
  await run(
    "docker",
    ["save", "-o", join(ctx, "sandbox.tar"), image],
    "ignore",
  );
  loader.succeed(`saved ${image}`);

  console.log();
  await run(
    "docker",
    [
      "build",
      "-f",
      DOCKERFILE,
      "--build-context",
      `sandbox-tar=${ctx}`,
      "--target",
      microvm ? "microvm" : "bundle",
      "-t",
      tag,
      __dirname,
    ],
    "inherit",
  );

  console.log(`\n  ${bright("✔")} bundled ${accent(tag)}\n`);
} catch (err) {
  console.error(
    `\n  ${red("✖")} ${err instanceof Error ? err.message : String(err)}\n`,
  );
  process.exitCode = 1;
} finally {
  rmSync(ctx, { recursive: true, force: true });
}
