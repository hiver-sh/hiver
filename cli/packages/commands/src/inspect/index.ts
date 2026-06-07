import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { mkdirSync, existsSync } from "node:fs";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";
import { dirname, resolve, join } from "node:path";
import { listSandboxes } from "@hiver.sh/client";
import { brand, accent, bright, bold, dim, red } from "../theme.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { createLoader } from "../hive.js";
import { confirm } from "../prompt.js";
import { EventRecorder } from "./recorder.js";

// packages/commands/{src,dist}/inspect → packages/devtools-server/dist/index.js.
// The built server also serves the built web client, so one process is enough.
const __dirname = dirname(fileURLToPath(import.meta.url));
const SERVER_ENTRY = resolve(
  __dirname,
  "../../../devtools-server/dist/index.js",
);
const BIN = resolve(__dirname, "../../bin.js"); // the `hiver` entry, for `up`
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const cli = withGateway(subcommand("inspect", "Launch the Hiver DevTools."))
  .option("--record", "record a trace to ~/.hive/traces")
  .option("--port <port>", "inspector server port", "5173");
run(cli);
const opts = cli.opts();
const recording = Boolean(opts.record);
const port = opts.port as string;
const serverUrl = `http://localhost:${port}`;
let gatewayUrl = resolveGatewayUrl(opts.gatewayUrl);

if (!existsSync(SERVER_ENTRY)) {
  console.error(
    `\n  ${red("✖")} devtools server not built — ${dim("run `npm run build` in cli/ first.")}\n`,
  );
  process.exit(1);
}

// With --record, a trace is written to ~/.hive/traces; the recorder itself is
// created once the (possibly newly-started) gateway URL is settled.
let tracePath: string | undefined;
if (recording) {
  const tracesDir = join(homedir(), ".hive", "traces");
  mkdirSync(tracesDir, { recursive: true });
  tracePath = join(tracesDir, `recording-${Date.now()}.jsonl`);
}

// The devtools UI is useless without the gateway, so check it's up first and
// point the user at `hiver up` if it isn't.
async function gatewayReachable(url: string): Promise<boolean> {
  try {
    await listSandboxes({ gatewayUrl: url, timeoutMs: 500 });
    return true;
  } catch {
    return false; // connection refused / timed out
  }
}

// Run `hiver up` (the CLI's own entry), inheriting stdio so its output shows.
function runUp(): Promise<boolean> {
  return new Promise((res) => {
    const child = spawn(process.execPath, [BIN, "up"], { stdio: "inherit" });
    child.on("error", () => res(false));
    child.on("exit", (code) => res(code === 0));
  });
}

// Ensure the gateway is up; if not, offer to start the stack (like the
// install-docker dialog), then wait for it to come online.
{
  const ping = createLoader(`checking gateway ${gatewayUrl}`).start();
  if (await gatewayReachable(gatewayUrl)) {
    ping.stop();
  } else {
    ping.fail(`gateway not reachable at ${gatewayUrl}`);

    if (
      !(await confirm(
        `  Start the local stack now with ${bright("hiver up")}?`,
      ))
    ) {
      console.error(`  ${dim("start it with")} ${bold("hiver up")}\n`);
      process.exit(1);
    }

    console.log();
    if (!(await runUp())) {
      console.error(`\n  ${red("✖")} could not start the stack\n`);
      process.exit(1);
    }

    // `up` may have published the gateway on a different port; re-resolve and
    // wait for it to answer (it already printed the URL, so this stays quiet).
    gatewayUrl = resolveGatewayUrl();
    const wait = createLoader("waiting for gateway").start();
    let ready = false;
    for (let i = 0; i < 20 && !ready; i++) {
      ready = await gatewayReachable(gatewayUrl);
      if (!ready) await sleep(500);
    }
    if (!ready) {
      wait.fail(`gateway still not reachable at ${gatewayUrl}`);
      process.exit(1);
    }
    wait.stop();
  }
}

// Gateway is ready — now show the banner.
console.log(
  `\n${bold(brand("DevTools"))}${recording ? " " + accent("[recording]") : ""}\n`,
);
console.log(`${dim("  inspector")} → ${serverUrl}`);
if (recording) console.log(`${dim("  trace")}     → ${tracePath}`);
console.log();

// Now that the gateway URL is settled, create the recorder if requested.
const recorder =
  recording && tracePath
    ? new EventRecorder(gatewayUrl, serverUrl, tracePath)
    : undefined;

function openBrowser(url: string) {
  const cmd =
    process.platform === "darwin"
      ? "open"
      : process.platform === "win32"
        ? "start"
        : "xdg-open";
  spawn(cmd, [url], { detached: true, stdio: "ignore" }).unref();
}

async function serverReachable(): Promise<boolean> {
  try {
    await fetch(serverUrl, { signal: AbortSignal.timeout(500) });
    return true;
  } catch {
    return false;
  }
}

let server: ReturnType<typeof spawn> | undefined;
let spinning = false;

if (await serverReachable()) {
  if (!recording) openBrowser(serverUrl);
} else {
  const loader = createLoader("starting devtools server…").start();
  spinning = true;

  server = spawn(process.execPath, [SERVER_ENTRY], {
    stdio: ["ignore", "pipe", "pipe"],
    env: { ...process.env, PORT: port, GATEWAY_URL: gatewayUrl },
  });

  // Read the server's output to detect readiness but don't print it — keep this
  // view to just the banner and status. Output is kept for error reporting.
  let serverOutput = "";
  let opened = false;
  for (const stream of [server.stdout, server.stderr]) {
    if (!stream) continue;
    const rl = createInterface({ input: stream, crlfDelay: Infinity });
    rl.on("line", (line) => {
      serverOutput += line + "\n";
      if (!opened && line.includes("DevTools server on")) {
        opened = true;
        const match = line.match(/https?:\/\/localhost:\d+/);
        const url = match?.[0] ?? serverUrl;
        loader.stop();
        spinning = false;
        if (!recording) openBrowser(url);
      }
    });
  }

  server.on("exit", (code) => {
    if (code !== null && code !== 0) {
      if (spinning) {
        loader.fail("server exited");
        spinning = false;
      }
      process.stderr.write(`server exited with code ${code}\n`);
      if (serverOutput.trim())
        process.stderr.write("\n" + serverOutput.trimEnd() + "\n");
    }
  });
}

// The recorder retries until the server answers, so it's safe to start now.
recorder?.start();

function shutdown() {
  if (spinning) spinning = false;
  recorder?.stop(); // writes the trace to disk
  server?.kill();
  process.exit(0);
}
process.on("SIGINT", shutdown);
process.on("SIGTERM", shutdown);
