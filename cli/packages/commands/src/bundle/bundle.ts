import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { basename, dirname, resolve, join } from "node:path";
import { brand, accent, bright, bold, dim } from "../theme.js";
import { createLoader } from "../hive.js";
import { imageExistsLocally, pullImage, isHiverBundle } from "../compose/images.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const DOCKERFILE = resolve(__dirname, "bundler.Dockerfile");
const BASE_IMAGE = "hiversh/core";

export interface BundleOptions {
  /** Resulting image tag. Defaults to `<image>-bundled`. */
  tag?: string;
  /** Bundle for the microvm backend (pre-builds the guest rootfs.ext4). */
  microvm?: boolean;
}

/** Whether `arg` points at a local directory (i.e. a Dockerfile context). */
export function isDirectory(arg: string): boolean {
  const resolved = resolve(arg);
  return existsSync(resolved) && statSync(resolved).isDirectory();
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

/**
 * Build (when `arg` is a directory with a Dockerfile) or pull (when `arg` is an
 * image ref) the input, then bundle it into a Hiver runtime image via
 * `bundler.Dockerfile`. Returns the resulting image tag.
 *
 * Throws on any failure, including when the input is already a Hiver bundle
 * (re-bundling would nest `hiversh/core` inside itself). Requires Docker — the
 * caller must have run `requireDocker()` first.
 */
export async function bundleImage(
  arg: string,
  opts: BundleOptions = {},
): Promise<string> {
  const resolvedArg = resolve(arg);
  const isDir = isDirectory(arg);
  if (isDir && !existsSync(join(resolvedArg, "Dockerfile"))) {
    throw new Error(`No Dockerfile found in ${resolvedArg}`);
  }
  const image = isDir
    ? basename(resolvedArg)
        .toLowerCase()
        .replace(/[^a-z0-9-]/g, "-")
    : arg;

  const tag = opts.tag ?? `${image.split(":")[0]}-bundled`;
  const microvm = Boolean(opts.microvm);

  // If the argument is a directory, build a Docker image from it first.
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
      throw new Error(`could not pull ${image}`);
    }
    pull.succeed(`Pulled ${image}`);
  }

  // Refuse to re-bundle: the label only lands on real bundles, so a positive
  // here is reliable and means the caller passed an already-bundled image.
  if (isHiverBundle(image)) {
    throw new Error(`${image} is already a Hiver bundle`);
  }

  if (!imageExistsLocally(BASE_IMAGE)) {
    const pull = createLoader(`Pulling ${BASE_IMAGE}`).start();
    const { ok, output } = await pullImage(BASE_IMAGE);
    if (!ok) {
      pull.fail(`could not pull ${BASE_IMAGE}`);
      if (output.trim()) process.stderr.write("\n" + output.trimEnd() + "\n");
      throw new Error(`could not pull ${BASE_IMAGE}`);
    }
    pull.succeed(`Pulled ${BASE_IMAGE}`);
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
    return tag;
  } finally {
    rmSync(ctx, { recursive: true, force: true });
  }
}
