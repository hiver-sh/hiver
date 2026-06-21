// Drive Playwright/Chromium inside the sandbox over HTTP, reusing one already-warm
// browser across commands.
//
// The playwright image ships a resident browser host (/opt/hiver/prewarm) that
// sbxguest starts before the microvm snapshot, so a claimed sandbox resumes with
// Chromium already launched. It exposes a `POST /eval` endpoint, reached through
// the sandbox ingress proxy; we feed it one JS command per request and read the
// command's console output back as the response body. The host keeps one
// process-wide REPL behind the endpoint, so the session is *stateful*: a binding
// declared in one command (e.g. `const scrape = ...`) is usable in the next — so
// you can define a helper in one request and call it in another. And because the
// browser is warmed at prewarm, the first command pays no
// require('playwright')/chromium.launch() cost.
//
// `browser`/`context`/`page` are shared warm instances seeded into the session —
// do NOT close them or call process.exit (that would tear down the resident
// host); just stop sending requests to end the session.
//
// Run with: npx tsx examples/playwright-exec-stream.ts
import * as hiver from "@hiver.sh/client";

const gatewayUrl = process.env.HIVER_GATEWAY_URL ?? "http://localhost:10000";
// TCP port the resident browser host's HTTP /eval endpoint listens on inside the
// sandbox; reached via the ingress proxy. Keep in sync with browser-host.cjs.
const HOST_PORT = Number(process.env.HIVER_BROWSER_PORT ?? "9223");

const tStart = performance.now();
const sandbox = await hiver.getOrCreateSandbox(
  "hiver-playwright-exec-stream",
  {
    image: "hiversh/playwright:microvm-23",
  },
  { gatewayUrl, timeoutMs: 120_000 },
);
const createMs = performance.now() - tStart;
console.info(`sandbox created in ${createMs.toFixed(0)}ms`);

const evalUrl = sandbox.proxyUrl(HOST_PORT) + "/eval";

// Under microvm isolation the warm browser is captured in the snapshot, so it
// answers the instant the sandbox is claimed — no readiness poll needed; the
// first eval below is the readiness edge.
console.info(`browser ready in ${(performance.now() - tStart).toFixed(0)}ms (create ${createMs.toFixed(0)}ms)`);

// Run one command in the stateful session: POST the JS to /eval and return its
// console output (the response body). State persists across calls, so `scrape`
// defined below is usable by the commands that follow it.
async function evalCmd(cmd: string): Promise<string> {
  const res = await sandbox.fetchImpl(evalUrl, {
    method: "POST",
    headers: { "content-type": "text/plain" },
    body: cmd,
    signal: AbortSignal.timeout(60_000),
  });
  if (!res.ok) throw new Error(`eval status ${res.status}`);
  return await res.text();
}

// Each command runs in the same stateful session, so `scrape` (and any pages it
// keeps) persist across calls. The browser is reused — never launched, never
// closed here.
const commands = [
  // Reuse the resident pre-opened `page` — a snapshot-resumed Chromium hangs on
  // browser.newPage() (new CDP target/renderer), so drive the warm page directly,
  // exactly as the resident-browser benchmark does.
  `const scrape = async (url) => { await page.goto(url, { waitUntil: 'domcontentloaded' }); const titles = await page.$$eval('.titleline > a', els => els.map(el => el.textContent)); for (const t of titles) console.log(t); }`,
  `await scrape('https://news.ycombinator.com')`,
  `await scrape('https://news.ycombinator.com/news?p=2')`,
];

for (const cmd of commands) {
  const out = await evalCmd(cmd);
  if (out) process.stdout.write(out);
}

console.info("\ndone.");
