import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { brand, dim, red } from "./theme.js";
import { createLoader } from "./hive.js";
import { confirm } from "./prompt.js";
import { resolveGatewayUrl } from "./args.js";

// The `hiver` entry, for spawning `up`. One level up from both src/ and dist/.
const BIN = resolve(dirname(fileURLToPath(import.meta.url)), "../bin.js");
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/**
 * Is the gateway actually serving HTTP? We do a real (but tiny) request rather
 * than a bare TCP connect: Docker publishes the host port the instant the
 * container starts — before the gateway process inside is listening — so a
 * connect would succeed too early and the next real request would fail with
 * "fetch failed". Any HTTP response (even a 404) proves the server is up; only
 * a connection error or timeout counts as not ready.
 */
export async function gatewayReachable(url: string): Promise<boolean> {
  try {
    await fetch(url, { signal: AbortSignal.timeout(1000) });
    return true;
  } catch {
    return false; // connection refused / reset / timed out
  }
}

/**
 * Whether a gateway runs on this machine. Only a local gateway shares the host
 * docker daemon, so image pulling/bundling is the CLI's job there; a remote
 * gateway pulls images itself. Unparseable URLs count as non-local.
 */
export function isLocalGateway(url: string): boolean {
  let host: string;
  try {
    host = new URL(url).hostname;
  } catch {
    return false;
  }
  return (
    host === "localhost" ||
    host === "127.0.0.1" ||
    host === "::1" ||
    host === "[::1]" ||
    host === "0.0.0.0"
  );
}

// Run `hiver up` (the CLI's own entry), inheriting stdio so its output shows.
function runUp(): Promise<boolean> {
  return new Promise((res) => {
    const child = spawn(process.execPath, [BIN, "up"], { stdio: "inherit" });
    child.on("error", () => res(false));
    child.on("exit", (code) => res(code === 0));
  });
}

/**
 * Ensure the local stack is running before a command talks to the gateway.
 * Pings the gateway; if it's down, offers to start it with `hiver up`, waits
 * for it to come online, and returns the (possibly re-resolved) gateway URL.
 * Exits the process if the user declines or the stack fails to come up.
 */
export async function ensureGateway(gatewayUrl: string): Promise<string> {
  const interactive = Boolean(process.stdout.isTTY);
  const ping = interactive
    ? createLoader(`checking gateway ${gatewayUrl}`).start()
    : null;
  if (await gatewayReachable(gatewayUrl)) {
    ping?.stop();
    return gatewayUrl;
  }
  ping?.fail(`gateway not reachable at ${gatewayUrl}`);
  if (!interactive) {
    process.stderr.write(`gateway not reachable at ${gatewayUrl}\n`);
  }

  if (
    !(await confirm(`  Start the local stack now with ${brand("hiver up")}?`))
  ) {
    console.error(`  ${dim("start it with")} ${brand("hiver up")}\n`);
    process.exit(1);
  }

  console.log();
  if (!(await runUp())) {
    console.error(`\n  ${red("✖")} could not start the stack\n`);
    process.exit(1);
  }

  // `up` may have published the gateway on a different port; re-resolve and
  // wait for it to answer (it already printed the URL, so this stays quiet).
  const resolved = resolveGatewayUrl();
  const wait = createLoader("waiting for gateway").start();
  let ready = false;
  for (let i = 0; i < 20 && !ready; i++) {
    ready = await gatewayReachable(resolved);
    if (!ready) await sleep(500);
  }
  if (!ready) {
    wait.fail(`gateway still not reachable at ${resolved}`);
    process.exit(1);
  }
  wait.stop();
  return resolved;
}
