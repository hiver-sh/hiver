import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import { mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";
import { dirname, resolve, join } from "node:path";
import ora, { type Ora } from "ora";
import { tag, brand, accent, bright, bold, dim } from "../theme.js";
import { EventRecorder } from "./recorder.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
// src/inspect → packages/commands (mirrored as dist/inspect → packages/commands).
const ROOT = resolve(__dirname, "../../..");

const SERVER_DIR = resolve(ROOT, "server");
const CLIENT_DIR = resolve(ROOT, "client");
const npm = process.platform === "win32" ? "npm.cmd" : "npm";

const args = process.argv.slice(2);
function getArg(name: string): string | undefined {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && i + 1 < args.length ? args[i + 1] : undefined;
}
const recording = args.includes("--record");
const serverUrl = getArg("server-url") ?? "http://localhost:3001";
const gatewayUrl = getArg("gateway-url");

// With --record, an EventRecorder polls the dev server (retrying until it's up)
// and writes a trace to ~/.hive/traces on shutdown.
let recorder: EventRecorder | undefined;
let tracePath: string | undefined;
if (recording) {
  const tracesDir = join(homedir(), ".hive", "traces");
  mkdirSync(tracesDir, { recursive: true });
  tracePath = join(tracesDir, `recording-${Date.now()}.json`);
  recorder = new EventRecorder(gatewayUrl, serverUrl, tracePath);
}

console.log(
  `\n${bold(brand("Sandbox Inspector"))}${recording ? " " + accent("[recording]") : ""}\n`,
);
console.log(`${dim("  server")}  → http://localhost:3001`);
console.log(`${dim("  client")}  → http://localhost:5173`);
if (recording) console.log(`${dim("  trace")}   → ${tracePath}`);
console.log();

const spinner: Ora = ora({
  text: "starting dev servers…",
  color: "magenta",
}).start();

// While the spinner owns the last terminal line, buffer child output so the
// animation isn't shredded by interleaved logs. Flush once it resolves.
const buffered: string[] = [];
function emit(text: string) {
  if (spinner.isSpinning) buffered.push(text);
  else process.stdout.write(text);
}
function flush() {
  if (buffered.length) {
    process.stdout.write(buffered.join(""));
    buffered.length = 0;
  }
}

function pipeLines(
  proc: ChildProcess,
  label: string,
  color: (s: string) => string,
  onLine?: (line: string) => void,
) {
  for (const stream of [proc.stdout, proc.stderr]) {
    if (!stream) continue;
    const rl = createInterface({ input: stream, crlfDelay: Infinity });
    rl.on("line", (line) => {
      emit(tag(label, color) + line + "\n");
      onLine?.(line);
    });
  }
}

function openBrowser(url: string) {
  const cmd =
    process.platform === "darwin"
      ? "open"
      : process.platform === "win32"
        ? "start"
        : "xdg-open";
  spawn(cmd, [url], { detached: true, stdio: "ignore" }).unref();
}

const server = spawn(npm, ["run", "dev"], {
  cwd: SERVER_DIR,
  stdio: ["ignore", "pipe", "pipe"],
  env: { ...process.env },
});
pipeLines(server, "server", brand);

let opened = false;
const client = spawn(npm, ["run", "dev"], {
  cwd: CLIENT_DIR,
  stdio: ["ignore", "pipe", "pipe"],
  env: { ...process.env },
});
pipeLines(client, "client", accent, (line) => {
  if (!opened && line.includes("Local:") && line.includes("localhost")) {
    opened = true;
    const match = line.match(/https?:\/\/localhost:\d+/);
    const url = match?.[0] ?? "http://localhost:5173";
    spinner.succeed(`Inspector ready — opening ${bright(url)}`);
    flush();
    openBrowser(url);
  }
});

// The recorder retries until the server answers, so it's safe to start now.
recorder?.start();

server.on("exit", (code) => {
  if (code !== null && code !== 0) {
    if (spinner.isSpinning) spinner.fail("server exited");
    flush();
    process.stderr.write(tag("server", brand) + `exited with code ${code}\n`);
  }
});
client.on("exit", (code) => {
  if (code !== null && code !== 0) {
    if (spinner.isSpinning) spinner.fail("client exited");
    flush();
    process.stderr.write(tag("client", accent) + `exited with code ${code}\n`);
  }
});

function shutdown() {
  if (spinner.isSpinning) spinner.stop();
  flush();
  recorder?.stop(); // writes the trace to disk
  server.kill();
  client.kill();
  process.exit(0);
}
process.on("SIGINT", shutdown);
process.on("SIGTERM", shutdown);
