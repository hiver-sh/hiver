// Uses a custom Docker image and connects to the sandbox via SSH with a TTY.
//
// Get a token with claude setup-token
// Then run with: CLAUDE_CODE_OAUTH_TOKEN='<token>' npx tsx examples/claude-code
import { spawn } from "node:child_process";
import { writeFile, unlink } from "node:fs/promises";
import { join, dirname } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import * as hive from "../../src";
import { createShutdown } from "../shutdown.js";

const claudeOAuthToken = process.env.CLAUDE_CODE_OAUTH_TOKEN;
if (!claudeOAuthToken) {
  console.error("CLAUDE_CODE_OAUTH_TOKEN is not set");
  process.exit(1);
}

const here = dirname(fileURLToPath(import.meta.url));
const sourceImage = "hive-example-claude-worker";
const imageTag = "hive-example-claude-worker-bundle";
const scriptPath = join(here, "../../../../scripts/bundle-images.sh");

console.log(`> Building image ${sourceImage}`);
// await buildImage(sourceImage, join(here, "image"));

console.log(`> Building sandbox bundle ${imageTag}`);
// await buildBundle(scriptPath, sourceImage, imageTag);

console.log("> Starting sandbox");
const sandbox = await hive.getOrCreateSandbox("hive-claude-code-worker-1", {
  ttl: 0,
  image: imageTag,
  env: {
    CLAUDE_CODE_OAUTH_TOKEN: claudeOAuthToken,
  },
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  egress: {
    allow: [
      { host: "api.anthropic.com" },
      { host: "platform.claude.com" },
      { host: "statsig.anthropic.com" },
      { host: "claude.ai" },
    ],
  },
});

const { shutdown } = createShutdown(sandbox);

const containerName = `hive-sandbox-${sandbox.id}`;
console.log(`> Looking up SSH port for ${containerName}`);

console.log(`> Waiting for sshd on ${sandbox.exposedEndpoint}`);
const sshPortParts = sandbox.exposedEndpoint!.split(':');
await waitForSsh(sshPortParts[0]!, sshPortParts[1]!);

await shutdown();

async function waitForSsh(host: string, port: string): Promise<void> {
  for (let attempt = 1; attempt <= 1000; attempt++) {
    try {
      return await sshConnect(host, port);
    } catch (err) {
      if (!(err instanceof Error) || !err.message.startsWith("ssh exit 255")) throw err;
      console.log(`> sshd not ready, retrying in 2s... (${attempt}/15)`);
      await new Promise(r => setTimeout(r, 2000));
    }
  }
}

async function sshConnect(host: string, port: string): Promise<void> {
  console.log('connecting', host, port)
  const askpass = join(tmpdir(), "hive-askpass.sh");
  await writeFile(askpass, "#!/bin/sh\necho root\n", { mode: 0o700 });
  try {
    await new Promise<void>((resolve, reject) => {
      const ssh = spawn(
        "ssh",
        [
          "-tt",
          "-p", port,
          "-o", "StrictHostKeyChecking=no",
          "-o", "UserKnownHostsFile=/dev/null",
          "-o", "LogLevel=ERROR",
          "-o", "PreferredAuthentications=password",
          `claude-agent@${host}`,
        ],
        {
          stdio: "inherit",
          env: {
            ...process.env,
            SSH_ASKPASS: askpass,
            SSH_ASKPASS_REQUIRE: "force",
            DISPLAY: process.env.DISPLAY ?? ":0",
          },
        },
      );
      ssh.once("error", reject);
      ssh.once("exit", (code) => {
        code === 0 || code === null ? resolve() : reject(new Error(`ssh exit ${code}`));
      });
    });
  } finally {
    await unlink(askpass).catch(() => {});
  }
}

function buildImage(tag: string, contextDir: string): Promise<void> {
  return spawnOk("docker", ["build", "-t", tag, contextDir]);
}

function buildBundle(
  scriptPath: string,
  sandboxImage: string,
  bundleTag: string,
): Promise<void> {
  return spawnOk("bash", [scriptPath, sandboxImage, bundleTag]);
}


function spawnOk(cmd: string, args: string[]): Promise<void> {
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
