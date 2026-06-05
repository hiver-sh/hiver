// Attach your local terminal to an interactive shell running inside the
// sandbox over a TTY exec stream — the standalone-CLI counterpart of the
// inspector's Terminal component (inspector/client/src/components/Terminal.tsx).
//
// Keystrokes are forwarded to the remote PTY in raw mode, the shell's output is
// streamed back to your terminal, and window resizes are propagated. Type
// `exit` (or Ctrl-D) inside the shell to quit.
//
// Run with: npx tsx examples/terminal-attach.ts
import process from "node:process";
import * as hive from "../src";
import { createShutdown } from "./shutdown.js";

const sandbox = await hive.getOrCreateSandbox("hive-terminal-attach", {
  image: "hive-node-sandbox",
  isolation: "microvm",
  entrypoint: "tail -f /dev/null",
  fs: [
    {
      backend: "local",
      mount: "/workspace",
      acls: [{ path: "/workspace/**", access: "rw" }],
    },
  ],
  ttl: 0,
});

const { stdin, stdout } = process;

// Restore the local terminal on the way out. createShutdown also handles
// SIGINT/SIGTERM and stops the sandbox.
const { shutdown } = createShutdown(sandbox, {
  cleanup: () => {
    if (stdin.isTTY) stdin.setRawMode(false);
    stdin.pause();
  },
});

// Open an interactive shell with a TTY. TERM/COLORTERM let the remote programs
// emit colour and cursor control like a real terminal, matching the inspector.
const exec = await sandbox.execStream("/bin/sh", {
  cwd: "/workspace",
  tty: true,
  env: { TERM: "xterm-256color", COLORTERM: "truecolor" },
});

// Push the current window size so the remote PTY starts at the right geometry,
// then keep it in sync. The server intercepts this CSI 8 sequence and resizes
// the PTY instead of forwarding it to the shell.
const sendResize = () =>
  exec
    .writeStdin(`\x1b[8;${stdout.rows ?? 24};${stdout.columns ?? 80}t`)
    .catch(() => {});
sendResize();
stdout.on("resize", sendResize);

console.error("[attached — type `exit` to quit]");

// Raw mode delivers every keystroke (arrows, Ctrl-C, tab) straight to the
// remote shell instead of being line-buffered or interpreted locally.
if (stdin.isTTY) stdin.setRawMode(true);
stdin.resume();
stdin.on("data", (buf: Buffer) => {
  exec.writeStdin(buf.toString("utf8")).catch(() => {});
});

// Stream the shell's output to the local terminal. In TTY mode the PTY merges
// stderr into stdout, so only stdout frames arrive.
for await (const pipe of exec.pipes) {
  if (pipe.stdout) stdout.write(pipe.stdout);
}

await shutdown(await exec.exitCode);
