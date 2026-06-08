#!/usr/bin/env node

import { existsSync } from "node:fs";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);

let cmd, cmdArgs;
if (process.env.DEV) {
  // Development: execute the TypeScript source through tsx.
  const tsxCandidates = [
    resolve(__dirname, "node_modules/.bin/tsx"),
    resolve(__dirname, "../node_modules/.bin/tsx"),
    resolve(__dirname, "../../node_modules/.bin/tsx"),
  ];
  const tsx = tsxCandidates.find((p) => existsSync(p));
  if (!tsx) {
    process.stderr.write("error: tsx not found — run `npm install` from the cli directory\n");
    process.exit(1);
  }
  cmd = tsx;
  cmdArgs = [resolve(__dirname, "src/cli.ts"), ...args];
} else {
  // Production: run the built JS directly with node.
  cmd = process.execPath;
  cmdArgs = [resolve(__dirname, "dist/cli.js"), ...args];
}

const child = spawn(cmd, cmdArgs, { stdio: "inherit" });

// The child shares our process group, so the terminal delivers Ctrl+C (and
// friends) to it directly. We must NOT die on those signals ourselves: long-
// running commands like `inspect` tear down asynchronously (restoring the
// cursor, killing the detached devtools server) and the shell only redraws its
// prompt once *we* exit. If the default signal action killed us first, the
// shell would reclaim the terminal mid-teardown — leaving the cursor hidden
// and arrow keys echoing as `^[[A`. Forward the signal and wait for the child
// to finish, then mirror how it exited.
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => {
    try {
      child.kill(sig);
    } catch {
      /* child already gone */
    }
  });
}
child.on("exit", (code, signal) => {
  if (signal) {
    // The child was killed by a signal — re-raise it so our exit status
    // reflects that (the child already restored the cursor before dying).
    // Drop our own handler first so the default action actually terminates us
    // instead of looping back into the forwarder.
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 0);
});
