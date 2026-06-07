import { spawn, spawnSync } from "node:child_process";
import { createInterface } from "node:readline/promises";
import { brand, bright, red, dim } from "./theme.js";

const DOCS = "https://docs.docker.com/get-docker/";

/** Whether the docker CLI is available on PATH. */
export function dockerAvailable(): boolean {
  return spawnSync("docker", ["--version"], { stdio: "ignore" }).status === 0;
}

interface Installer {
  label: string;
  cmd: string;
  args: string[];
}

// Platform-appropriate install command. macOS/Windows install Docker Desktop
// via the system package manager; Linux uses Docker's convenience script.
function installerFor(platform: NodeJS.Platform): Installer | undefined {
  switch (platform) {
    case "darwin":
      return { label: "brew install --cask docker", cmd: "brew", args: ["install", "--cask", "docker"] };
    case "linux":
      return {
        label: "curl -fsSL https://get.docker.com | sh",
        cmd: "sh",
        args: ["-c", "curl -fsSL https://get.docker.com | sh"],
      };
    case "win32":
      return {
        label: "winget install Docker.DockerDesktop",
        cmd: "winget",
        args: [
          "install", "-e", "--id", "Docker.DockerDesktop",
          "--accept-package-agreements", "--accept-source-agreements",
        ],
      };
    default:
      return undefined;
  }
}

async function confirm(question: string): Promise<boolean> {
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  try {
    const answer = await rl.question(`${question} ${dim("(y/N)")} `);
    return /^y(es)?$/i.test(answer.trim());
  } finally {
    rl.close();
  }
}

function runInstaller(inst: Installer): Promise<boolean> {
  return new Promise((resolve) => {
    const child = spawn(inst.cmd, inst.args, { stdio: "inherit" });
    child.on("error", () => resolve(false)); // the installer tool itself is missing
    child.on("exit", (code) => resolve(code === 0));
  });
}

function bail(message: string): never {
  console.error(`  ${dim(message)}\n`);
  process.exit(1);
}

/**
 * Ensure docker is available. If it isn't, offer to install it for the current
 * platform; on accept, run the installer and re-check. Exits otherwise.
 */
export async function requireDocker(): Promise<void> {
  if (dockerAvailable()) return;

  console.error(`\n  ${red("✖")} Docker not found.`);

  const inst = installerFor(process.platform);
  if (!inst) bail(`Install Docker manually: ${DOCS}`);
  // Can't prompt without a TTY (CI, pipes) — point to the docs instead.
  if (!process.stdin.isTTY) bail(`Install Docker and retry: ${DOCS}`);

  const accepted = await confirm(`  Install it now with ${bright(inst.label)}?`);
  if (!accepted) bail(`No problem — install Docker manually: ${DOCS}`);

  console.log(`\n  ${brand("Installing Docker")} ${dim(inst.label)}\n`);
  if (!(await runInstaller(inst))) {
    bail(`Installation failed — install manually: ${DOCS}`);
  }

  // Docker Desktop (macOS/Windows) often needs a first launch before the CLI works.
  if (!dockerAvailable()) {
    bail("Docker installed but not ready yet — start Docker, then retry.");
  }

  console.log(`  ${bright("✔")} Docker ready\n`);
}
