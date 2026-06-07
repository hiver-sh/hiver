import { spawn, spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

// Expand `${VAR}` / `${VAR:-default}` the way docker compose does, against the
// current environment.
function expandEnv(value: string): string {
  return value.replace(
    /\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}/g,
    (_, name: string, fallback: string | undefined) =>
      process.env[name] ?? fallback ?? "",
  );
}

/** The `image:` refs declared in a compose file, with env vars expanded. */
export function imagesFromCompose(composeFile: string): string[] {
  const text = readFileSync(composeFile, "utf8");
  return [...text.matchAll(/^\s*image:\s*(\S+)/gm)].map((m) => expandEnv(m[1]));
}

/** Whether an image exists in the local docker image store. */
export function imageExistsLocally(image: string): boolean {
  return (
    spawnSync("docker", ["image", "inspect", image], { stdio: "ignore" })
      .status === 0
  );
}

// hiversh/core's entrypoint — every bundle is `FROM hiversh/core` and inherits
// it, so it doubles as a bundle signal for images predating the label.
const SANDBOXD_ENTRYPOINT = "/usr/local/bin/sandboxd";

/**
 * Whether a (locally present) image is a Hiver bundle. Detected by the
 * `hiver.bundle` label stamped by `hiver bundle`, falling back to the inherited
 * `sandboxd` entrypoint so bundles built before the label still register. Both
 * are read in one `docker inspect`; returns `false` if the image isn't present.
 */
export function isHiverBundle(image: string): boolean {
  const res = spawnSync(
    "docker",
    [
      "image",
      "inspect",
      "-f",
      '{{index .Config.Labels "hiver.bundle"}}|{{json .Config.Entrypoint}}',
      image,
    ],
    { encoding: "utf8" },
  );
  if (res.status !== 0) return false;
  const [label, entrypoint = ""] = res.stdout.trim().split("|");
  return label === "1" || entrypoint.includes(SANDBOXD_ENTRYPOINT);
}

/**
 * Verify the stack's images are present locally. Returns the subset that is
 * missing (empty array ⇒ all available).
 */
export function missingImages(composeFile: string): string[] {
  return imagesFromCompose(composeFile).filter(
    (image) => !imageExistsLocally(image),
  );
}

/**
 * Pull an image with `docker pull`. Resolves with whether it succeeded and the
 * captured output (shown by the caller on failure).
 */
export function pullImage(
  image: string,
): Promise<{ ok: boolean; output: string }> {
  return new Promise((resolve) => {
    let output = "";
    const child = spawn("docker", ["pull", image], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    child.stdout?.on("data", (d: Buffer) => (output += d));
    child.stderr?.on("data", (d: Buffer) => (output += d));
    child.on("error", (err) =>
      resolve({ ok: false, output: output + err.message }),
    );
    child.on("exit", (code) => resolve({ ok: code === 0, output }));
  });
}
