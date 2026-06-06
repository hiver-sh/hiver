import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { dirname, resolve, join } from "node:path";
import ora from "ora";
import { brand, accent, bright, bold, dim, red } from "../theme.js";

// src/bundle (and dist/bundle) → repo root, where docker/ lives.
const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(__dirname, "../../../../..");
const DOCKERFILE = "docker/bundler.Dockerfile";

// Args after the command name (`hiver bundle <image> ...`).
const args = process.argv.slice(3);
function getArg(name: string): string | undefined {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && i + 1 < args.length ? args[i + 1] : undefined;
}
const image = args.find((a) => !a.startsWith("--"));
const tag =
  getArg("tag") ?? (image ? `${image.split(":")[0]}-bundled` : undefined);

if (!image || !tag) {
  console.error(
    `\n  ${red("✖")} missing image — ${dim("usage: hiver bundle <image> [--tag <runtime-tag>]")}\n`,
  );
  process.exit(1);
}

// Run a command from the repo root; reject on a non-zero exit.
function run(
  cmd: string,
  cmdArgs: string[],
  stdio: "ignore" | "inherit",
): Promise<void> {
  return new Promise((res, rej) => {
    const child = spawn(cmd, cmdArgs, { cwd: REPO_ROOT, stdio });
    child.on("error", rej);
    child.on("exit", (code) =>
      code === 0 ? res() : rej(new Error(`${cmd} exited with code ${code}`)),
    );
  });
}

console.log(
  `\n${bold(brand("Bundle"))} ${accent(image)} ${dim("→")} ${bright(tag)}\n`,
);

// The bundler reads sandbox.tar from a build context dir, so save into a temp
// dir and pass it through as `sandbox-tar`.
const ctx = mkdtempSync(join(tmpdir(), "hiver-bundle-"));
try {
  const spinner = ora({ text: `saving ${image}…`, color: "magenta" }).start();
  await run(
    "docker",
    ["save", "-o", join(ctx, "sandbox.tar"), image],
    "ignore",
  );
  spinner.succeed(`saved ${image}`);

  console.log();
  await run(
    "docker",
    [
      "build",
      "-f",
      DOCKERFILE,
      "--build-context",
      `sandbox-tar=${ctx}`,
      "-t",
      tag,
      ".",
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
