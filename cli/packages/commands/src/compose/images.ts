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
