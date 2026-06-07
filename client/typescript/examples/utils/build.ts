import { spawn } from "node:child_process";

/**
 * Build a sandbox image bundle from a local image directory using the hiver
 * CLI (`hiver bundle <dir> --tag <tag>`). The resulting `tag` can be passed as
 * the sandbox config's `image`.
 */
export function buildBundle(
  sandboxImage: string,
  bundleTag: string,
): Promise<void> {
  return spawnOk("hiver", ["bundle", sandboxImage, "--tag", bundleTag]);
}

export function spawnOk(cmd: string, args: string[]): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { stdio: "inherit" });
    child.once("error", reject);
    child.once("exit", (code: number | null) =>
      code === 0
        ? resolve()
        : reject(new Error(`${cmd} ${args[0]}: exit ${code}`)),
    );
  });
}
