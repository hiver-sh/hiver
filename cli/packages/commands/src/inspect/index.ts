import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { mkdirSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve, join } from "node:path";
import { listSandboxes } from "@hiver.sh/client";
import { HIVER_DIR, readConfig } from "../config.js";
import { brand, dim, red } from "../theme.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { createLoader, hex } from "../hive.js";
import { ensureGateway } from "../gateway.js";
import { EventRecorder } from "./recorder.js";

// packages/commands/{src,dist}/inspect → packages/inspector-server/dist/index.js.
// The built server also serves the built web client, so one process is enough.
const __dirname = dirname(fileURLToPath(import.meta.url));
const SERVER_ENTRY = resolve(
  __dirname,
  "../../../inspector-server/dist/index.js",
);

const cli = withGateway(subcommand("inspect", "Launch the Hiver DevTools."))
  .argument("[key]", "open the inspector on this sandbox (opens the home view when omitted)")
  .option("--record", "record a trace to ~/.hiver/traces")
  .option("--port <port>", "inspector server port", "5173");
run(cli);
const opts = cli.opts();
const recording = Boolean(opts.record);
const port = opts.port as string;
const serverUrl = `http://localhost:${port}`;
const key = cli.args[0] as string | undefined;
// Resolved below (after the gateway is up): the client routes by <id>/<key>, so
// a key alone isn't enough — we look up the sandbox's id first.
let inspectPath = "";
let resolvedSandbox: { id: string; key: string } | undefined;

if (recording && !key) {
  console.error(
    `  ${red("✖")} --record requires a sandbox key: hiver inspect <key> --record\n`,
  );
  process.exit(1);
}
let gatewayUrl = resolveGatewayUrl(opts.gatewayUrl);

if (!existsSync(SERVER_ENTRY)) {
  console.error(
    `\n  ${red("✖")} devtools server not built — ${dim("run `npm run build` in cli/ first.")}\n`,
  );
  process.exit(1);
}

// With --record, a trace is written to ~/.hiver/traces; the recorder itself is
// created once the (possibly newly-started) gateway URL is settled.
let tracePath: string | undefined;
if (recording) {
  const tracesDir = join(HIVER_DIR, "traces");
  mkdirSync(tracesDir, { recursive: true });
  tracePath = join(tracesDir, `recording-${Date.now()}.jsonl`);
}

// The devtools UI is useless without the gateway, so make sure the stack is up
// first (offering to start it), and pick up any re-resolved URL.
gatewayUrl = await ensureGateway(gatewayUrl);

// Gateway is ready. When a key was given, deep-link straight to its view —
// resolve the key to its routing id first (the client routes by <id>/<key>, and
// in pack mode many keys share one id). The client uses a HashRouter, so the
// route lives in the URL fragment. If the key isn't found, open the home view.
if (key) {
  try {
    const sandbox = (await listSandboxes({ gatewayUrl })).find(
      (s) => s.key === key,
    );
    if (sandbox) {
      resolvedSandbox = sandbox;
      inspectPath = `/#/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(key)}`;
    } else {
      console.error(
        `  ${red("✖")} no sandbox with key ${dim(key)}${recording ? "" : " — opening the home view"}\n`,
      );
      if (recording) process.exit(1);
    }
  } catch (err) {
    console.error(
      `  ${red("✖")} could not resolve ${dim(key)} (${err instanceof Error ? err.message : String(err)})${recording ? "" : " — opening the home view"}\n`,
    );
    if (recording) process.exit(1);
  }
}

// Gateway is ready — now show where things are.
console.log();
console.log(
  `${hex(0.82)} ${brand("Hiver")} ${dim("Inspector")} → ${serverUrl}${inspectPath}`,
);
if (recording)
  console.log(
    `${hex(0.82)} ${brand("Hiver")} ${dim("trace")}     → ${tracePath}`,
  );
console.log();

// Now that the gateway URL is settled, create the recorder if requested.
const recorder =
  recording && tracePath
    ? new EventRecorder(gatewayUrl, serverUrl, tracePath, resolvedSandbox)
    : undefined;

function openBrowser(url: string) {
  const cmd =
    process.platform === "darwin"
      ? "open"
      : process.platform === "win32"
        ? "start"
        : "xdg-open";
  // Best-effort: on headless/Linux hosts the opener (e.g. xdg-open) may be
  // missing. Swallow the spawn 'error' so a failed auto-open doesn't crash the
  // process via an unhandled 'error' event — the URL is already printed.
  const child = spawn(cmd, [url], { detached: true, stdio: "ignore" });
  child.on("error", () => {});
  child.unref();
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
  if (!recording) openBrowser(serverUrl + inspectPath);
} else {
  const loader = createLoader("starting devtools server…").start();
  spinning = true;

  // Pin GATEWAY_URL for the server only when this run resolved to something
  // other than the saved config (an explicit --gateway-url, or a port the
  // gateway moved to). Left unset for a plain `hiver connect` gateway, the
  // server reads it live from config on each page load — so a still-running
  // server reflects a later `hiver connect` instead of a value frozen here.
  const pinnedGateway =
    gatewayUrl !== readConfig().gatewayUrl ? { GATEWAY_URL: gatewayUrl } : {};

  // detached: own process group, so we can kill the server *and* anything it
  // ever spawns as one unit, and so the terminal's SIGINT goes only to us
  // (we forward the teardown deliberately in shutdown).
  server = spawn(process.execPath, [SERVER_ENTRY], {
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
    env: { ...process.env, PORT: port, ...pinnedGateway },
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
        if (!recording) openBrowser(url + inspectPath);
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

// Kill the server's whole process group (negative pid) so neither it nor any
// descendant is left listening on the port. Falls back to the bare child if
// the group is already gone, and is safe to call repeatedly.
function killServer(signal: NodeJS.Signals) {
  if (!server?.pid) return;
  try {
    process.kill(-server.pid, signal);
  } catch {
    try {
      server.kill(signal);
    } catch {
      /* already dead */
    }
  }
}

let shuttingDown = false;
function shutdown() {
  if (shuttingDown) return; // idempotent — signals can fire more than once
  shuttingDown = true;
  // Restore the cursor right away — don't make it wait out the server-kill
  // grace period below (it's hidden while the startup spinner is mid-flight).
  if (process.stdout.isTTY) process.stdout.write("\x1b[?25h");
  if (spinning) spinning = false;
  recorder?.stop(); // writes the trace to disk
  killServer("SIGTERM"); // ask nicely…
  // …then force it and quit. The short grace lets the server release the port
  // cleanly; SIGKILL guarantees it can't outlive us if it ignores SIGTERM.
  setTimeout(() => {
    killServer("SIGKILL");
    process.exit(0);
  }, 300).unref();
}
process.on("SIGINT", shutdown); // Ctrl+C
process.on("SIGTERM", shutdown); // kill
process.on("SIGHUP", shutdown); // terminal closed (common on Linux/SSH)
// Backstop: force-kill synchronously on any exit path we didn't catch above
// (normal exit, uncaught error) so the inspector never outlives us.
process.on("exit", () => killServer("SIGKILL"));

// Stay in the foreground until interrupted. The detached child no longer keeps
// our event loop alive on its own, and when the server was already running we
// never spawned one — without this the process would exit immediately.
const keepAlive = setInterval(() => {}, 1 << 30);
process.on("exit", () => clearInterval(keepAlive));
