import { mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { EventRecorder } from "./recorder.js";

const R = "\x1b[0m";
const DIM = "\x1b[2m";
const BOLD = "\x1b[1m";
const YELLOW = "\x1b[33m";

const args = process.argv.slice(2);
function getArg(name: string): string | undefined {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && i + 1 < args.length ? args[i + 1] : undefined;
}

const controllerUrl = getArg("gateway-url");
const serverUrl = getArg("server-url") ?? "http://localhost:3001";
const tracesDir = join(homedir(), ".hive", "traces");
mkdirSync(tracesDir, { recursive: true });
const outputPath = join(tracesDir, `recording-${Date.now()}.json`);

const recorder = new EventRecorder(controllerUrl, serverUrl, outputPath);

console.log(`\n${BOLD}Sandbox Inspector${R} ${YELLOW}[recorder]${R}\n`);
if (controllerUrl) console.log(`${DIM}  gateway${R}    → ${controllerUrl}`);
console.log(`${DIM}  server${R}     → ${serverUrl}`);
console.log(`${DIM}  output${R}     → ${outputPath}`);
console.log();

process.on("exit", () => recorder.stop());
process.on("SIGINT", () => process.exit(0));
process.on("SIGTERM", () => process.exit(0));

recorder.start();
process.stdout.write(`${YELLOW}[recorder]${R} waiting for ${serverUrl} …\n`);

// Keep the event loop alive until signalled
process.stdin.resume();
