// Benchmark the *resident browser host* path against the same lifecycle that
// nottelabs/browserarena's hello-browser bench measures for cloud browser
// providers: create → connect → goto → release. The playwright image ships
// /opt/hiver/prewarm, which sbxguest starts before the microvm snapshot, so a
// claimed sandbox resumes with Chromium already running. Instead of spawning
// `node -e "require('playwright'); chromium.launch()"` per exec (see
// benchmark-browser-ready.ts — ~0.9s require + ~1.1s launch + node startup), the
// client drives the already-warm browser over plain HTTP: each command is a
// `POST /eval` to the resident host's endpoint through the sandbox ingress proxy
// (/v1/<key>/proxy/<port>/eval) — no exec session and no per-request in-guest
// process spawn (the old path bridged a Unix socket with `socat` over
// execStream). The host keeps one persistent REPL behind the endpoint, so
// top-level bindings persist across requests.
//
// Stages (mirroring browserarena/src/benchmarks/hello-browser):
//   create   getOrCreateSandbox — the provider "create session" API call.
//   connect  GET the resident host's HTTP endpoint (through the proxy) until it
//            answers 200 — the analog of chromium.connectOverCDP().
//   goto     navigate the resident page to BENCH_URL and wait for
//            `domcontentloaded` — the "open page" stage, page.goto({ waitUntil:
//            "domcontentloaded" }). This is the best proxy for browser speed.
//   release  sandbox.shutdown() — the provider "release session" API call.
//
// Run with: npx tsx examples/benchmark-browser-resident.ts
//   HIVER_GATEWAY_URL=http://<host>:10000   BENCH_RUNS=10   BENCH_URL=https://example.com
// Sweep the load with BENCH_MIN_QPS / BENCH_MAX_QPS — the number of concurrent
// requests dispatched per second. The benchmark runs one batch of BENCH_RUNS
// runs at each integer QPS from MIN to MAX (inclusive), waiting BENCH_WAIT_MS
// (default 60s) between levels so the warm pool can replenish. E.g. RUNS=10,
// MIN_QPS=1, MAX_QPS=10 → 10 batches of 10 runs, one per QPS level. Within a
// batch, QPS=1 fires one request per second; QPS>1 launches that many runs on
// each one-second tick without waiting for in-flight runs to finish. Each
// batch's sandboxes are released (sandbox.shutdown) during this cooldown — off
// the measured create→connect→goto path — rather than inline per run.
//
// Requires an image built with the resident host (Dockerfile/browser-host.cjs)
// and an sbxguest with the prewarm hook.
import * as hiver from "@hiver.sh/client";

const gatewayUrl = process.env.HIVER_GATEWAY_URL ?? "http://localhost:10000";
const RUNS = Number(process.env.BENCH_RUNS ?? "10");
const MIN_QPS = Number(process.env.BENCH_MIN_QPS ?? "1");
const MAX_QPS = Number(process.env.BENCH_MAX_QPS ?? "1");
const WAIT_MS = Number(process.env.BENCH_WAIT_MS ?? "10000");
// Per-stage deadline: a run whose /eval (or connect health-check) doesn't return
// within this (e.g. a wedged page.goto when a contended --single-process Chromium
// stalls under load) is aborted and counted as a failed run, rather than blocking
// the whole sweep forever on Promise.all. Generous vs. a healthy ~0.5s round-trip.
const RUN_TIMEOUT_MS = Number(process.env.BENCH_RUN_TIMEOUT_MS ?? "20000");
// browserarena navigates every provider to example.com and waits for
// `domcontentloaded`; keep the same default so timings are comparable.
const URL = process.env.BENCH_URL ?? "https://example.com";
// TCP port the resident browser host's HTTP /eval endpoint listens on inside the
// sandbox; reached via the ingress proxy. Keep in sync with browser-host.cjs.
const HOST_PORT = Number(process.env.BENCH_HOST_PORT ?? "9223");
const sandboxConfig: hiver.SandboxConfig = {
  image: "hiversh/playwright:microvm-23",
};

// The "open page" / goto stage: navigate the resident (pre-opened) page to URL
// and wait for `domcontentloaded` (matches browserarena's page.goto(url, {
// waitUntil: "domcontentloaded" })). With BENCH_NAV_TIMING on (default) the
// command also reads the browser's Navigation Timing so we can split the cost
// into DNS / connect / TTFB / response / DOM — but that second page.evaluate is
// itself a post-navigation round-trip into a fresh execution context, so it
// inflates the measured goto (it lands in `resid`). Set BENCH_NAV_TIMING=0 to
// drop it and measure the bare page.goto; compare the two `goto` columns to see
// how much of the overhead is the instrumentation vs page.goto's own lag.
// Emitted as one line: `goto:<status>` or `goto:<status> <navTimingJson>`.
const NAV_TIMING = process.env.BENCH_NAV_TIMING !== "0";
const gotoPage = !NAV_TIMING
  ? `console.log('goto:' + (await page.goto(${JSON.stringify(URL)}, { waitUntil: 'domcontentloaded' })).status())`
  : `const __r = await page.goto(${JSON.stringify(URL)}, { waitUntil: 'domcontentloaded' }); ` +
    `const __n = await page.evaluate(() => { const e = performance.getEntriesByType('navigation')[0]; ` +
    `return e ? { dns: e.domainLookupEnd - e.domainLookupStart, connect: e.connectEnd - e.connectStart, ` +
    `ttfb: e.responseStart - e.requestStart, response: e.responseEnd - e.responseStart, ` +
    `dom: e.domContentLoadedEventEnd - e.responseEnd, nav: e.domContentLoadedEventEnd - e.startTime } : null; }); ` +
    `console.log('goto:' + __r.status() + ' ' + JSON.stringify(__n))`;

// In-browser Navigation Timing for the goto, in ms (all deltas off the same
// navigation entry). `nav` is the browser-measured navigation total.
interface NavTiming {
  dns: number;
  connect: number;
  ttfb: number;
  response: number;
  dom: number;
  nav: number;
}

// A no-op REPL round-trip: one POST /eval through the proxy → eval →
// console.log → response body, with zero browser work. This is the pure
// transport+REPL floor; `gotoMs - nav - pingMs` is then what the goto costs
// *over* that floor (mostly the instrumentation page.evaluate + node scheduling).
const PONG = "__PONG__";
const pingHost = `console.log('${PONG}')`;

interface Run {
  createMs: number; // provider "create session" (getOrCreateSandbox)
  connectMs: number; // HTTP attach to resident host (connectOverCDP analog)
  pingMs: number; // no-op REPL round-trip — the transport+REPL floor
  gotoMs: number; // page.goto(URL, domcontentloaded) — the "open page" stage
  releaseMs: number; // provider "release session" (sandbox.shutdown) — filled at cooldown
  totalMs: number; // lifecycle sum, completed once release lands (releaseBatch)
  nav?: NavTiming; // in-browser navigation breakdown (egress diagnostic)
}

// A completed create→connect→goto run whose sandbox is still alive: its release
// (sandbox.shutdown) is deferred to the cooldown between QPS levels, where
// releaseBatch tears the sandbox down and fills in run.releaseMs / run.totalMs.
interface PendingRun {
  i: number; // 1-based run index, for the per-run log line
  run: Run;
  sandbox: hiver.Sandbox;
}

// Run one REPL command over HTTP: POST the JS to the resident host's /eval
// endpoint through the sandbox ingress proxy and return the command's console
// output (the response body). Bounded by RUN_TIMEOUT_MS so a wedged stage fails
// the run rather than hanging the sweep.
async function evalCmd(
  sandbox: hiver.Sandbox,
  cmd: string,
  label: string,
): Promise<string> {
  const res = await sandbox.fetchImpl(sandbox.proxyUrl(HOST_PORT) + "/eval", {
    method: "POST",
    headers: { "content-type": "text/plain" },
    body: cmd,
    signal: AbortSignal.timeout(RUN_TIMEOUT_MS),
  });
  if (!res.ok) throw new Error(`${label}: eval status ${res.status}`);
  return await res.text();
}

// Poll the resident host's HTTP endpoint until it answers 200 — the connect /
// attach stage. On the warm microvm path it's up at claim (first try); on the
// cold container path browser-host.cjs is still launching Chromium, so poll
// (≤ RUN_TIMEOUT_MS) until it's listening.
async function waitHealthy(
  sandbox: hiver.Sandbox,
  label: string,
): Promise<void> {
  const url = sandbox.proxyUrl(HOST_PORT) + "/healthz";
  const start = performance.now();
  for (;;) {
    try {
      const res = await sandbox.fetchImpl(url, {
        method: "GET",
        signal: AbortSignal.timeout(2_000),
      });
      if (res.ok) {
        await res.text();
        return;
      }
    } catch {
      /* endpoint not up yet — retry below */
    }
    if (performance.now() - start > RUN_TIMEOUT_MS) {
      throw new Error(`${label} timed out after ${RUN_TIMEOUT_MS}ms`);
    }
    await sleep(100);
  }
}

// One full create → connect → goto → release lifecycle (browserarena's
// hello-browser stages). Returns the per-stage timings, or null if a command
// failed. Safe to run concurrently — it appends the created sandbox to the
// `sandboxes` array the caller passes in as a teardown safety net, and removes
// it again once it has released the sandbox itself.
async function doRun(
  i: number,
  sandboxes: hiver.Sandbox[],
): Promise<PendingRun | null> {
  // create: the provider "create session" API call.
  const t0 = performance.now();
  const sandbox = await hiver.getOrCreateSandbox(
    `bench-resident-${Date.now()}-${i}`,
    sandboxConfig,
    { gatewayUrl, timeoutMs: 120_000 },
  );
  sandboxes.push(sandbox);
  const createMs = performance.now() - t0;

  // connect: poll the resident host's HTTP endpoint until it answers — the analog
  // of chromium.connectOverCDP() (attaching to the running browser). On the warm
  // microvm path the host is in the snapshot and answers on the first try, so
  // connectMs measures the warm attach; on the cold container path it includes
  // the browser launch (no resident browser to attach to).
  const t1 = performance.now();
  await waitHealthy(sandbox, "connect");
  const connectMs = performance.now() - t1;

  let run: Run | null = null;

  // ping: a no-op REPL round-trip with zero browser work — measures the pure
  // transport+REPL floor (one /eval POST through the proxy) so we can attribute
  // how much of the goto's overhead is transport vs browser.
  const tp = performance.now();
  await evalCmd(sandbox, pingHost, "ping");
  const pingMs = performance.now() - tp;

  // goto: navigate the pre-opened page to URL and wait for `domcontentloaded` —
  // the "open page" stage and the best proxy for browser speed.
  const t2 = performance.now();
  const body = await evalCmd(sandbox, gotoPage, `goto ${i}`);
  const gotoMs = performance.now() - t2;
  // The response body is the command's console output. Expected: a line
  // `goto:<status> <navTimingJson>`. Parse the status word and the Navigation
  // Timing JSON (may be `null` if the entry wasn't available).
  const out = body.split("\n");
  const goto = out.find((l) => l.startsWith("goto:"));
  if (!goto) {
    console.error(`  run ${i}: goto failed: ${JSON.stringify(out)}`);
  } else {
    const rest = goto.slice("goto:".length);
    const sp = rest.indexOf(" ");
    let nav: NavTiming | undefined;
    try {
      const parsed = sp === -1 ? null : JSON.parse(rest.slice(sp + 1));
      if (parsed) nav = parsed as NavTiming;
    } catch {
      /* keep nav undefined if the breakdown didn't parse */
    }
    run = {
      createMs,
      connectMs,
      pingMs,
      gotoMs,
      releaseMs: 0,
      totalMs: 0,
      nav,
    };
  }

  // No release here: shutdown is deferred to the cooldown between QPS levels
  // (releaseBatch) so it runs off the measured create→connect→goto path and
  // doesn't contend with the other in-flight runs in this batch. A failed run
  // leaves its sandbox in the `sandboxes` safety net to be torn down then too; a
  // successful one is handed back (still alive) and dropped from that net.
  if (!run) return null;
  const idx = sandboxes.indexOf(sandbox);
  if (idx !== -1) sandboxes.splice(idx, 1);
  // Print live progress as each run completes (release/total land later, in the
  // cooldown) so a long batch shows movement instead of looking stuck between the
  // dispatch line and the cooldown.
  console.info(
    `  run ${i}: connect ${run.connectMs.toFixed(0)}ms, goto ${run.gotoMs.toFixed(0)}ms ` +
      `(create ${run.createMs.toFixed(0)}ms)`,
  );
  return { i, run, sandbox };
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// The outcome of one QPS batch: the successful runs (whose sandboxes are still
// alive, pending release in the cooldown) and any sandboxes from runs that
// failed before completing (torn down in the cooldown too).
interface BatchResult {
  pending: PendingRun[];
  leaked: hiver.Sandbox[];
}

// Run one batch of RUNS requests at `qps` concurrent requests per second: each
// tick fires a batch of up to `qps` runs without awaiting them, then waits 1s
// before the next tick. All in-flight runs are awaited; the sandboxes are NOT
// released here — that is deferred to releaseBatch in the cooldown.
async function runBatch(qps: number): Promise<BatchResult> {
  console.info(
    `\n=== qps ${qps}: dispatching ${RUNS} runs at ${qps} req/s ===`,
  );
  const pending: PendingRun[] = [];
  const leaked: hiver.Sandbox[] = [];
  const inFlight: Promise<void>[] = [];

  for (let dispatched = 0; dispatched < RUNS; ) {
    const batch = Math.min(qps, RUNS - dispatched);
    for (let j = 0; j < batch; j++) {
      const i = dispatched + j + 1;
      inFlight.push(
        doRun(i, leaked)
          .then((pr) => {
            if (pr) pending.push(pr);
          })
          .catch((e) => console.error(`  run ${i}: error: ${e}`)),
      );
    }
    dispatched += batch;
    if (dispatched < RUNS) await sleep(1000);
  }

  await Promise.all(inFlight);
  return { pending, leaked };
}

// Release a batch's sandboxes during the cooldown: shut every sandbox down (off
// the measured request path) and, for each successful run, record its release
// latency and complete its lifecycle total. Failed runs' leaked sandboxes are
// torn down too. Shutdowns run concurrently so teardown overlaps the cooldown
// wait rather than serializing N releases — so releaseMs reflects a release
// under concurrent teardown, not an isolated one. Returns the batch's runs (now
// with releaseMs/totalMs filled) for the summary.
async function releaseBatch(batch: BatchResult): Promise<Run[]> {
  await Promise.all([
    ...batch.pending.map(async ({ i, run, sandbox }) => {
      const t = performance.now();
      try {
        // await sandbox.shutdown();
      } catch (e) {
        console.error(`  run ${i}: release error: ${e}`);
      }
      run.releaseMs = performance.now() - t;
      run.totalMs =
        run.createMs + run.connectMs + run.pingMs + run.gotoMs + run.releaseMs;
    }),
    ...batch.leaked.map((s) => s.shutdown().catch(() => {})),
  ]);
  return batch.pending.map((p) => p.run);
}

const avg = (a: number[]) => a.reduce((x, y) => x + y, 0) / a.length;
const min = (a: number[]) => Math.min(...a);
const max = (a: number[]) => Math.max(...a);
// browserarena's headline number is the median of per-run totals — report
// median alongside min/avg/max so our numbers are directly comparable.
const median = (a: number[]) => {
  const s = [...a].sort((x, y) => x - y);
  const m = Math.floor(s.length / 2);
  return s.length % 2 ? s[m]! : (s[m - 1]! + s[m]!) / 2;
};
const row = (label: string, a: number[]) =>
  `  ${label.padEnd(15)} ${min(a).toFixed(0).padStart(5)}ms  ${median(a).toFixed(0).padStart(5)}ms  ${avg(a).toFixed(0).padStart(5)}ms  ${max(a).toFixed(0).padStart(5)}ms`;

// Sweep QPS from MIN to MAX inclusive — one batch per level — with a WAIT_MS
// cooldown between levels so the warm pool can replenish before the next load.
interface Batch {
  qps: number;
  results: Run[];
}
const batches: Batch[] = [];

for (let qps = MIN_QPS; qps <= MAX_QPS; qps++) {
  const batch = await runBatch(qps);

  // Release this batch's sandboxes during the cooldown rather than inline in the
  // run: kick the (concurrent) shutdowns off and let them overlap the WAIT_MS
  // wait, so teardown stays off the measured create→connect→goto path and out of
  // the next load level. releaseBatch fills in each run's releaseMs/totalMs, so
  // it must settle before the summary reads them. The last level still releases
  // (final cleanup) but skips the wait.
  const releaseP = releaseBatch(batch);
  if (qps < MAX_QPS) {
    console.info(
      `\ncooldown: releasing ${batch.pending.length} sandboxes + waiting ${(WAIT_MS / 1000).toFixed(0)}s ...`,
    );
    await sleep(WAIT_MS); // release runs concurrently during the wait
  }
  const results = await releaseP; // ensure teardown finished + run fields filled
  batches.push({ qps, results });

  if (results.length === 0) {
    console.error(`  qps ${qps}: no successful runs.`);
  } else {
    console.info(`\n=== qps ${qps} summary (${results.length} runs) ===`);
    console.info(`                  min     med     avg     max`);
    console.info(
      row(
        "create",
        results.map((r) => r.createMs),
      ),
    );
    console.info(
      row(
        "connect",
        results.map((r) => r.connectMs),
      ),
    );
    console.info(
      row(
        "ping",
        results.map((r) => r.pingMs),
      ),
    );
    console.info(
      row(
        "goto",
        results.map((r) => r.gotoMs),
      ),
    );
    console.info(
      row(
        "release",
        results.map((r) => r.releaseMs),
      ),
    );
    console.info(
      row(
        "total",
        results.map((r) => r.totalMs),
      ),
    );
  }
}

console.info("\ndone.");

// Combined report: one row per QPS level (median ms — `total` is directly
// comparable to browserarena's headline median total latency).
console.info(`\n=== sweep summary (median ms per qps level) ===`);
console.info(`  qps   runs   create  connect    ping    goto  release   total`);
for (const { qps, results } of batches) {
  if (results.length === 0) {
    console.info(
      `  ${String(qps).padStart(3)}      0       —       —       —       —       —       —`,
    );
    continue;
  }
  console.info(
    `  ${String(qps).padStart(3)}   ${String(results.length).padStart(4)}   ` +
      `${median(results.map((r) => r.createMs))
        .toFixed(0)
        .padStart(6)}  ` +
      `${median(results.map((r) => r.connectMs))
        .toFixed(0)
        .padStart(6)}  ` +
      `${median(results.map((r) => r.pingMs))
        .toFixed(0)
        .padStart(6)}  ` +
      `${median(results.map((r) => r.gotoMs))
        .toFixed(0)
        .padStart(6)}  ` +
      `${median(results.map((r) => r.releaseMs))
        .toFixed(0)
        .padStart(6)}  ` +
      `${median(results.map((r) => r.totalMs))
        .toFixed(0)
        .padStart(6)}`,
  );
}

// Navigation breakdown: split the goto into the browser's network phases (DNS,
// TCP connect, TTFB, response, DOM), `nav` (browser-measured navigation total),
// and the overhead `xport = gotoMs − nav`. That overhead is further split into
// `ping` (the no-op transport+REPL floor) and `resid = xport − ping` (the rest:
// the instrumentation page.evaluate + node scheduling). If DNS/connect/TTFB
// dominate, egress is the bottleneck; if `ping` dominates, it's the tunnel
// round-trip; if `dom`/`resid` dominate, it's browser/node CPU.
console.info(`\n=== goto breakdown (median ms per qps level) ===`);
console.info(
  `  qps   runs     dns  connect    ttfb  response     dom     nav    ping   resid    goto`,
);
for (const { qps, results } of batches) {
  const withNav = results.filter((r) => r.nav);
  if (withNav.length === 0) {
    console.info(
      `  ${String(qps).padStart(3)}      0       (no navigation timing)`,
    );
    continue;
  }
  const m = (pick: (n: NavTiming) => number) =>
    median(withNav.map((r) => pick(r.nav!)))
      .toFixed(0)
      .padStart(6);
  const ping = median(withNav.map((r) => r.pingMs));
  const resid = median(withNav.map((r) => r.gotoMs - r.nav!.nav - r.pingMs));
  console.info(
    `  ${String(qps).padStart(3)}   ${String(withNav.length).padStart(4)}   ` +
      `${m((n) => n.dns)}  ${m((n) => n.connect)}  ${m((n) => n.ttfb)}  ` +
      `${m((n) => n.response)}  ${m((n) => n.dom)}  ${m((n) => n.nav)}  ` +
      `${ping.toFixed(0).padStart(6)}  ${resid.toFixed(0).padStart(6)}  ` +
      `${median(withNav.map((r) => r.gotoMs))
        .toFixed(0)
        .padStart(6)}`,
  );
}

if (batches.every((b) => b.results.length === 0)) {
  console.error("\nno successful runs.");
  process.exit(1);
}
