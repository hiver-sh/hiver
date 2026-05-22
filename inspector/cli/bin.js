#!/usr/bin/env node
// Runs the TypeScript CLI via tsx — no build step required.
import { existsSync } from "node:fs";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));

const tsxCandidates = [
  resolve(__dirname, "node_modules/.bin/tsx"),
  resolve(__dirname, "../node_modules/.bin/tsx"),
];
const tsx = tsxCandidates.find((p) => existsSync(p));
if (!tsx) {
  process.stderr.write("error: tsx not found — run `npm install` from the inspector directory\n");
  process.exit(1);
}
const entry = resolve(__dirname, "src/index.ts");

const child = spawn(tsx, [entry, ...process.argv.slice(2)], {
  stdio: "inherit",
});
child.on("exit", (code) => process.exit(code ?? 0));
