// Drive Playwright/Chromium inside the sandbox over a single exec stream,
// reusing one already-warm browser across commands.
//
// The playwright image ships a resident browser host (/opt/hiver/prewarm) that
// sbxguest starts before the microvm snapshot, so a claimed sandbox resumes with
// Chromium already launched. We open one long-lived session to it by bridging an
// exec stream to its Unix socket with `socat`, then feed it commands via
// `writeStdin`. Because the host runs a REPL bound to that socket, the session
// is *stateful*: a binding declared in one command (e.g. `const page = ...`) is
// usable in the next — so you can create a page in one request and click it in
// another. And because the browser is warmed at prewarm, the first command pays
// no require('playwright')/chromium.launch() cost.
//
// The host prints a `__READY__` line after each command settles (and on
// connect); we wait for it before sending the next. `browser`/`context` are
// shared warm instances seeded into the session — do NOT close them or call
// process.exit here (that would tear down the resident host); end the session by
// just disconnecting (we abort the stream).
//
// Run with: npx tsx examples/playwright-exec-stream.ts
import * as hiver from "@hiver.sh/client";

const gatewayUrl = process.env.HIVER_GATEWAY_URL ?? "http://localhost:10000";
const MARKER = "__READY__";

const tStart = performance.now();
const sandbox = await hiver.getOrCreateSandbox(
  "hiver-playwright-exec-stream",
  {
    image: "hiversh/playwright:microvm-10",
    cpu: 2,
    memory: 2048,
  },
  { gatewayUrl, timeoutMs: 120_000 },
);
const createMs = performance.now() - tStart;
console.info(`sandbox created in ${createMs.toFixed(0)}ms`);

// Bridge the exec stream to the resident host's socket. socat is a dumb byte
// pipe: our stdin → socket, socket → our stdout. The browser is already warm on
// the other side, so there's no launch step in the commands below.
const ac = new AbortController();
const exec = await sandbox.execStream(
  ["socat", "-", "UNIX-CONNECT:/run/hiver/browser.sock"],
  { signal: ac.signal },
);
// We end the session by aborting the stream, which rejects exitCode with
// AbortError; that's expected, so mark it handled to avoid an unhandled rejection.
void exec.exitCode.catch(() => {});

// Each command runs in the same stateful session, so `scrape` (and any pages it
// keeps) persist across calls. The browser is reused — never launched, never
// closed here.
const commands = [
  // Define a reusable scraper; it persists in the session for later commands.
  `const scrape = async (url) => { const page = await browser.newPage(); await page.goto(url); const titles = await page.$$eval('.titleline > a', els => els.map(el => el.textContent)); await page.close(); for (const t of titles) console.log(t); }`,
  // Reuse the same warm browser for multiple pages.
  `await scrape('https://news.ycombinator.com')`,
  `await scrape('https://news.ycombinator.com/news?p=2')`,
];

let next = 0;
let started = false;
let buffer = "";
// Consume output line by line: a `__READY__` marker means the session is idle.
// Send the next command on each marker; once they're exhausted, disconnect.
for await (const pipe of exec.pipes) {
  if (pipe.stderr) process.stderr.write(pipe.stderr);
  if (!pipe.stdout) continue;
  buffer += pipe.stdout;
  let nl: number;
  while ((nl = buffer.indexOf("\n")) !== -1) {
    const line = buffer.slice(0, nl);
    buffer = buffer.slice(nl + 1);
    if (line === MARKER) {
      // First marker = session live: the resident warm browser is reachable.
      // This is the "time to start" — no require/launch was paid here.
      if (!started) {
        started = true;
        console.info(
          `browser ready in ${(performance.now() - tStart).toFixed(0)}ms ` +
            `(create ${createMs.toFixed(0)}ms + connect ${(performance.now() - tStart - createMs).toFixed(0)}ms)`,
        );
      }
      if (next < commands.length) {
        await exec.writeStdin(commands[next++] + "\n");
      } else {
        ac.abort(); // all commands done — end the session (don't exit the host).
      }
    } else {
      console.log(line);
    }
  }
  if (ac.signal.aborted) break;
}

console.info("\ndone.");
