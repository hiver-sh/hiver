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
child.on("exit", (code) => process.exit(code ?? 0));
