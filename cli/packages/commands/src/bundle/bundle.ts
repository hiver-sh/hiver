import { spawn, spawnSync } from "node:child_process";
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { basename, dirname, resolve, join } from "node:path";
import { brand, accent, bright, bold, dim } from "../theme.js";
import { createLoader } from "../hive.js";
import {
  imageExistsLocally,
  pullImage,
  isHiverBundle,
  SANDBOXD_ENTRYPOINT,
} from "../compose/images.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const DOCKERFILE = resolve(__dirname, "bundler.Dockerfile");
const BASE_IMAGE = "hiversh/core";

// Multi-arch builds need a docker-container driver builder; the default `docker`
// driver can't build for a foreign arch and push a manifest list. We create one
// on demand and reuse it across runs.
const MULTIARCH_BUILDER = "hiver-multiarch";

export interface BundleOptions {
  /** Resulting image tag. Defaults to `<image>-bundled`. */
  tag?: string;
  /** Bundle for the microvm backend (pre-builds the guest rootfs.ext4). */
  microvm?: boolean;
  /**
   * Target platforms (e.g. `["linux/amd64", "linux/arm64"]`). When set, the
   * bundle is built using the multiarch builder. Multiple platforms require
   * `push: true` since Docker cannot store multi-arch images locally.
   */
  platforms?: string[];
  /** Push the result to the registry instead of loading it locally. */
  push?: boolean;
}

/**
 * Whether a `docker save`/`type=docker` tar at `tarPath` is a Hiver bundle.
 * Reads the image config's labels and entrypoint from inside the tar (the same
 * signals as `isHiverBundle`, which can't be used here because the multi-arch
 * path never loads the image into the local docker store). Returns `false` on
 * any read/parse failure.
 */
function isHiverBundleTar(tarPath: string): boolean {
  const manifest = spawnSync("tar", ["-xOf", tarPath, "manifest.json"], {
    encoding: "utf8",
  });
  if (manifest.status !== 0) return false;
  let configPath: string | undefined;
  try {
    configPath = (JSON.parse(manifest.stdout) as { Config?: string }[])[0]
      ?.Config;
  } catch {
    return false;
  }
  if (!configPath) return false;
  const config = spawnSync("tar", ["-xOf", tarPath, configPath], {
    encoding: "utf8",
  });
  if (config.status !== 0) return false;
  try {
    const cfg = (
      JSON.parse(config.stdout) as {
        config?: { Labels?: Record<string, string>; Entrypoint?: string[] };
      }
    ).config;
    if (cfg?.Labels?.["hiver.bundle"] === "1") return true;
    return (cfg?.Entrypoint ?? []).some((e) => e.includes(SANDBOXD_ENTRYPOINT));
  } catch {
    return false;
  }
}

/**
 * The repository part of an image reference, dropping a trailing `:tag`. The
 * tag separator must come after the last `/`, so a registry host:port (e.g.
 * `localhost:5000/foo`) isn't mistaken for a tag. The registry prefix — and
 * thus a custom registry — is preserved.
 */
function repoOf(ref: string): string {
  const colon = ref.lastIndexOf(":");
  return colon > ref.lastIndexOf("/") ? ref.slice(0, colon) : ref;
}

/** The host's docker platform, e.g. `linux/arm64`. */
function hostPlatform(): string {
  const res = spawnSync(
    "docker",
    ["version", "-f", "{{.Server.Os}}/{{.Server.Arch}}"],
    { encoding: "utf8" },
  );
  const out = res.status === 0 ? res.stdout.trim() : "";
  return out || "linux/amd64";
}

/** Ensure a docker-container builder exists for multi-arch builds. */
function ensureMultiarchBuilder(): void {
  const exists =
    spawnSync("docker", ["buildx", "inspect", MULTIARCH_BUILDER], {
      stdio: "ignore",
    }).status === 0;
  if (exists) return;
  const res = spawnSync(
    "docker",
    [
      "buildx",
      "create",
      "--name",
      MULTIARCH_BUILDER,
      "--driver",
      "docker-container",
      "--bootstrap",
    ],
    { stdio: "inherit" },
  );
  if (res.status !== 0) {
    throw new Error(`failed to create buildx builder "${MULTIARCH_BUILDER}"`);
  }
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
  const target = microvm ? "microvm" : "bundle";

  // Builds that target a specific platform (or need to push) go through the
  // multiarch builder. The local single-arch path below is unchanged for
  // `hiver start` (no --platform, no --push).
  if (opts.push || opts.platforms?.length) {
    const platforms = opts.platforms?.length
      ? opts.platforms
      : [hostPlatform()];
    return bundleMultiArch({
      image,
      isDir,
      resolvedArg,
      tag,
      target,
      platforms,
      push: Boolean(opts.push),
    });
  }

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
        target,
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

interface MultiArchArgs {
  image: string;
  isDir: boolean;
  resolvedArg: string;
  tag: string;
  target: string;
  platforms: string[];
  push: boolean;
}

/**
 * Build the bundle once per platform and stitch the results into a single
 * manifest list pushed as `tag`.
 *
 * Each platform is handled independently because the bundle bakes in a tar of
 * the input image, which is platform-specific — so there's no single buildx
 * invocation that can produce all arches. For each platform we export the input
 * for that arch as a `type=docker` tar (the exact layout bundler.Dockerfile
 * unpacks) straight from the buildx builder, build the bundler for that arch
 * (pushing by digest, no tag), then `imagetools create` combines the per-arch
 * digests under `tag`.
 *
 * The export sources layers from the builder's cache rather than a `docker
 * pull` + `docker save` round-trip, so an input that was just built by
 * `make publish-images` isn't re-downloaded. `hiversh/core` (the `FROM` base)
 * and any image input must still be published multi-arch so the missing arches
 * resolve.
 */
async function bundleMultiArch(args: MultiArchArgs): Promise<string> {
  const { image, isDir, resolvedArg, tag, target, platforms, push } = args;

  if (!push && platforms.length > 1) {
    throw new Error(
      `building for multiple platforms requires --push (Docker cannot store multi-arch images locally)`,
    );
  }

  ensureMultiarchBuilder();
  const repo = repoOf(tag);

  console.log(
    `\n${bold(brand("Bundle"))} ${accent(image)} ${dim("→")} ${bright(tag)} ${dim(`(${platforms.join(", ")})`)}\n`,
  );

  if (!push) {
    // Single platform, no push: export input tar then build with --load.
    const [platform] = platforms;
    const ctx = mkdtempSync(join(tmpdir(), "hiver-bundle-"));
    try {
      const tarPath = join(ctx, "sandbox.tar");
      const prepArgs = [
        "buildx",
        "build",
        "--builder",
        MULTIARCH_BUILDER,
        "--platform",
        platform,
        "-o",
        `type=docker,dest=${tarPath}`,
      ];
      if (isDir) {
        prepArgs.push(resolvedArg);
      } else {
        const fromFile = join(ctx, "from.Dockerfile");
        writeFileSync(fromFile, `FROM ${image}\n`);
        prepArgs.push("-f", fromFile, ctx);
      }
      console.log(`${dim("→")} preparing ${accent(`${image} (${platform})`)}`);
      await run("docker", prepArgs, "inherit");

      if (isHiverBundleTar(tarPath)) {
        throw new Error(`${image} is already a Hiver bundle`);
      }

      await run(
        "docker",
        [
          "buildx",
          "build",
          "-f",
          DOCKERFILE,
          "--build-context",
          `sandbox-tar=${ctx}`,
          "--target",
          target,
          "--platform",
          platform,
          "--builder",
          MULTIARCH_BUILDER,
          "--load",
          "-t",
          tag,
          __dirname,
        ],
        "inherit",
      );
    } finally {
      rmSync(ctx, { recursive: true, force: true });
    }

    console.log(`\n  ${bright("✔")} bundled ${accent(tag)}\n`);
    return tag;
  }

  const sources: string[] = [];
  for (const [i, platform] of platforms.entries()) {
    const ctx = mkdtempSync(join(tmpdir(), "hiver-bundle-"));
    try {
      // Export the input for this platform as a docker-format tar. type=docker
      // emits exactly the layout bundler.Dockerfile unpacks (manifest.json
      // .Layers + blobs/), and buildkit sources the layers from the builder
      // cache instead of a fresh pull. A directory builds its own Dockerfile;
      // an image ref exports via a one-line `FROM`.
      const tarPath = join(ctx, "sandbox.tar");
      const prepArgs = [
        "buildx",
        "build",
        "--builder",
        MULTIARCH_BUILDER,
        "--platform",
        platform,
        "-o",
        `type=docker,dest=${tarPath}`,
      ];
      if (isDir) {
        prepArgs.push(resolvedArg);
      } else {
        const fromFile = join(ctx, "from.Dockerfile");
        writeFileSync(fromFile, `FROM ${image}\n`);
        prepArgs.push("-f", fromFile, ctx);
      }
      console.log(`${dim("→")} preparing ${accent(`${image} (${platform})`)}`);
      await run("docker", prepArgs, "inherit");

      // The re-bundle guard reads the markers from the tar (the image is never
      // loaded into the local store); only need to check once.
      if (i === 0 && isHiverBundleTar(tarPath)) {
        throw new Error(`${image} is already a Hiver bundle`);
      }

      const metaFile = join(ctx, "meta.json");
      await run(
        "docker",
        [
          "buildx",
          "build",
          "-f",
          DOCKERFILE,
          "--build-context",
          `sandbox-tar=${ctx}`,
          "--target",
          target,
          "--platform",
          platform,
          "--builder",
          MULTIARCH_BUILDER,
          // Push the per-arch image without a tag; we tag the manifest list
          // below. `push=true` is required — `push-by-digest` only controls how
          // it's pushed, not whether, so without it the digest never reaches the
          // registry and `imagetools create` can't find it.
          "--output",
          `type=image,name=${repo},push=true,push-by-digest=true,name-canonical=true`,
          "--metadata-file",
          metaFile,
          __dirname,
        ],
        "inherit",
      );

      const meta = JSON.parse(readFileSync(metaFile, "utf8")) as Record<
        string,
        string
      >;
      const digest = meta["containerimage.digest"];
      if (!digest) {
        throw new Error(`no image digest reported for ${platform}`);
      }
      sources.push(`${repo}@${digest}`);
    } finally {
      rmSync(ctx, { recursive: true, force: true });
    }
  }

  const loader = createLoader(`pushing ${tag}`).start();
  try {
    await run(
      "docker",
      ["buildx", "imagetools", "create", "-t", tag, ...sources],
      "inherit",
    );
  } catch (err) {
    loader.fail(`could not push ${tag}`);
    throw err;
  }
  loader.succeed(`pushed ${tag}`);

  console.log(`\n  ${bright("✔")} bundled ${accent(tag)}\n`);
  return tag;
}
