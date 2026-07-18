import { spawn, execSync } from "node:child_process";
import { createInterface } from "node:readline";
import { mkdirSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve, join } from "node:path";
import { listSandboxes } from "@hiver.sh/client";
import { HIVER_DIR } from "../config.js";
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

// The gateway a server already listening on `port` is serving, read from the
// `__HIVE_GATEWAY_URL__` global it injects into its HTML. Works across server
// versions, and reflects a still-running server frozen to an old `hiver connect`
// (or pinned via a past --gateway-url) — the exact case where reusing it would
// make the inspector talk to a different gateway than the CLI now resolves.
async function runningGateway(): Promise<string | undefined> {
  try {
    const html = await (
      await fetch(serverUrl, { signal: AbortSignal.timeout(1000) })
    ).text();
    const m = html.match(/__HIVE_GATEWAY_URL__=("(?:[^"\\]|\\.)*")/);
    return m ? (JSON.parse(m[1]) as string) : undefined;
  } catch {
    return undefined;
  }
}

// Kill whatever holds `port` (unix only; best-effort) and wait for it to let go,
// so we can start a fresh server there. Used to evict a stale inspector serving
// the wrong gateway.
async function freePort(p: string): Promise<void> {
  if (process.platform !== "win32") {
    try {
      const pids = execSync(`lsof -ti tcp:${p}`, {
        stdio: ["ignore", "pipe", "ignore"],
      })
        .toString()
        .trim()
        .split(/\s+/)
        .filter(Boolean);
      for (const pid of pids) {
        try {
          process.kill(Number(pid), "SIGTERM");
        } catch {
          /* already gone */
        }
      }
    } catch {
      /* lsof missing, or nothing on the port */
    }
  }
  for (let i = 0; i < 30 && (await serverReachable()); i++) {
    await new Promise((r) => setTimeout(r, 100));
  }
}

let server: ReturnType<typeof spawn> | undefined;
let spinning = false;

// Reuse an already-running inspector only when it serves the gateway the CLI
// resolved; otherwise it's stale (a past `hiver inspect` frozen to an older
// `hiver connect`), so evict it and start fresh. Without this, the reuse path
// never re-applies the gateway and the inspector keeps talking to the old one.
let reuse = false;
if (await serverReachable()) {
  if ((await runningGateway()) === gatewayUrl) {
    reuse = true;
  } else {
    await freePort(port);
  }
}

if (reuse) {
  if (!recording) openBrowser(serverUrl + inspectPath);
} else {
  const loader = createLoader("starting devtools server…").start();
  spinning = true;

  // The server resolves its gateway live from ~/.hiver/config.json on every page
  // load, so a freshly spawned *or* reused inspector always reflects the gateway
  // the CLI currently sees — the last `hiver connect`, else the built-in default.
  // Only an explicit `--gateway-url` (a one-off override we never persist to
  // config) pins the server to a fixed value. In every other case we must not let
  // an ambient GATEWAY_URL leak into the child and shadow the config the CLI reads
  // — that is exactly what made the inspector talk to a different gateway than the
  // CLI. A moved port needs no pin: `hiver up` saves it to config first.
  const env: NodeJS.ProcessEnv = { ...process.env, PORT: port };
  if (opts.gatewayUrl) env.GATEWAY_URL = gatewayUrl;
  else delete env.GATEWAY_URL;

  // detached: own process group, so we can kill the server *and* anything it
  // ever spawns as one unit, and so the terminal's SIGINT goes only to us
  // (we forward the teardown deliberately in shutdown).
  server = spawn(process.execPath, [SERVER_ENTRY], {
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
    env,
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
