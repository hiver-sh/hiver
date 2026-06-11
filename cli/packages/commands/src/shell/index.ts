import { listSandboxes, type ExecProcess } from "@hiver.sh/client";
import { white, dim, red } from "../theme.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { ensureGateway } from "../gateway.js";

const cmd = withGateway(subcommand("shell", "Open an interactive shell in a sandbox."))
  .argument("<sandbox-key>", "sandbox to connect to")
  .option("--command <cmd>", "command to run (default: /bin/sh or entrypoint TTY)");
run(cmd);

const key = cmd.args[0];
const { gatewayUrl: gatewayFlag, command } = cmd.opts();
let gatewayUrl = resolveGatewayUrl(gatewayFlag);
gatewayUrl = await ensureGateway(gatewayUrl);

const sandbox = (await listSandboxes({ gatewayUrl })).find((s) => s.key === key);
if (!sandbox) {
  console.error(`\n  ${red("✖")} no sandbox with key ${white(key)} on ${dim(gatewayUrl)}\n`);
  process.exit(1);
}

const config = await sandbox.getConfig().catch(() => null);

const ac = new AbortController();

let exec: ExecProcess;
if (!command && config?.tty === true) {
  // Attach to the entrypoint's TTY (empty command routes to its pty)
  exec = await sandbox.execStream("", { signal: ac.signal });
} else {
  const shell = command ?? "/bin/sh";
  const cwd = config?.cwd ?? (config?.fs?.[0] as { mount?: string } | undefined)?.mount ?? undefined;
  exec = await sandbox.execStream(shell, {
    tty: true,
    cwd,
    env: { TERM: process.env.TERM ?? "xterm-256color", COLORTERM: "truecolor" },
    signal: ac.signal,
  });
}

exec.exitCode.catch(() => {});

if (process.stdin.isTTY) process.stdin.setRawMode(true);
process.stdin.resume();

process.stdin.on("data", (chunk: Buffer) => {
  exec.writeStdin(chunk.toString()).catch(() => {});
});

function sendResize() {
  const cols = process.stdout.columns ?? 80;
  const rows = process.stdout.rows ?? 24;
  exec.writeStdin(`\x1b[8;${rows};${cols}t`).catch(() => {});
}

if (process.stdout.isTTY) {
  process.stdout.on("resize", sendResize);
  sendResize();
}

function cleanup() {
  if (process.stdin.isTTY) process.stdin.setRawMode(false);
  process.stdin.pause();
  ac.abort();
}

process.on("SIGTERM", () => {
  cleanup();
  process.exit(0);
});

for await (const pipe of exec.pipes) {
  if (pipe.stdout) process.stdout.write(pipe.stdout);
  if (pipe.stderr) process.stderr.write(pipe.stderr);
}

cleanup();
const code = await exec.exitCode.catch(() => 1);
process.exit(typeof code === "number" ? code : 0);
