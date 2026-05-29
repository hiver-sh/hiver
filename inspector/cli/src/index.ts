import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(__dirname, "../..");

const SERVER_DIR = resolve(ROOT, "server");
const CLIENT_DIR = resolve(ROOT, "client");
const npm = process.platform === "win32" ? "npm.cmd" : "npm";

const R = "\x1b[0m";
const DIM = "\x1b[2m";
const BOLD = "\x1b[1m";
const CYAN = "\x1b[36m";
const MAGENTA = "\x1b[35m";
const GREEN = "\x1b[32m";

function tag(label: string, color: string) {
  return `${color}[${label}]${R} `;
}

function pipeLines(
  proc: ChildProcess,
  label: string,
  color: string,
  onLine?: (line: string) => void,
) {
  for (const stream of [proc.stdout, proc.stderr]) {
    if (!stream) continue;
    const rl = createInterface({ input: stream, crlfDelay: Infinity });
    rl.on("line", (line) => {
      process.stdout.write(tag(label, color) + line + "\n");
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

console.log(`\n${BOLD}Sandbox Inspector${R}\n`);
console.log(`${DIM}  server${R}  → http://localhost:3001`);
console.log(`${DIM}  client${R}  → http://localhost:5173`);
console.log();

const server = spawn(npm, ["run", "dev"], {
  cwd: SERVER_DIR,
  stdio: ["ignore", "pipe", "pipe"],
  env: { ...process.env },
});
pipeLines(server, "server", CYAN);

const client = spawn(npm, ["run", "dev"], {
  cwd: CLIENT_DIR,
  stdio: ["ignore", "pipe", "pipe"],
  env: { ...process.env },
});

let opened = false;
pipeLines(client, "client", MAGENTA, (line) => {
  if (!opened && line.includes("Local:") && line.includes("localhost")) {
    opened = true;
    const match = line.match(/https?:\/\/localhost:\d+/);
    const url = match?.[0] ?? "http://localhost:5173";
    console.log(`\n${GREEN}Opening${R} ${url}\n`);
    openBrowser(url);
  }
});

server.on("exit", (code) => {
  if (code !== null && code !== 0)
    process.stderr.write(tag("server", CYAN) + `exited with code ${code}\n`);
});
client.on("exit", (code) => {
  if (code !== null && code !== 0)
    process.stderr.write(tag("client", MAGENTA) + `exited with code ${code}\n`);
});

process.on("SIGINT", () => {
  server.kill();
  client.kill();
  process.exit(0);
});
process.on("SIGTERM", () => {
  server.kill();
  client.kill();
  process.exit(0);
});
