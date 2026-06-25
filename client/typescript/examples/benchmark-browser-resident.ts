// Benchmark the *resident browser* path against the same lifecycle that
// nottelabs/browserarena's hello-browser bench measures for cloud browser
// providers: create → connect → goto → release. The browser image keeps a
// headless Chromium resident with its CDP endpoint open (see
// docker/browser/chromehost), captured warm in the microvm snapshot, so a
// claimed sandbox resumes with Chromium already listening. The client attaches
// exactly like the browser-cdp.ts example: chromium.connectOverCDP() to the
// in-guest relay's STABLE /cdp url through the sandbox ingress proxy
// (/v1/<key>/proxy/<port>/cdp), then drives the warm page with the normal
// Playwright API — no per-exec `node -e "chromium.launch()"` (see
// benchmark-browser-ready.ts: ~0.9s require + ~1.1s launch + node startup).
//
// Stages (mirroring browserarena/src/benchmarks/hello-browser):
//   create   getOrCreateSandbox — the provider "create session" API call.
//   connect  chromium.connectOverCDP() to the resident browser through the proxy
//            (retried until it answers) — the "attach" stage.
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
// Requires the CDP browser image (docker/browser/chromehost) and an
// sbxguest with the prewarm hook.
import * as hiver from "@hiver.sh/client";
import { chromium, type Browser } from "playwright-core";

const gatewayUrl = process.env.HIVER_GATEWAY_URL ?? "http://localhost:10000";
const RUNS = Number(process.env.BENCH_RUNS ?? "10");
const MIN_QPS = Number(process.env.BENCH_MIN_QPS ?? "1");
const MAX_QPS = Number(process.env.BENCH_MAX_QPS ?? "1");
const WAIT_MS = Number(process.env.BENCH_WAIT_MS ?? "10000");
// Per-stage deadline: a run whose connect/goto doesn't return within this (e.g. a
// wedged page.goto when a contended --single-process Chromium stalls under load)
// is aborted and counted as a failed run, rather than blocking the whole sweep
// forever on Promise.all. Generous vs. a healthy ~0.5s round-trip.
const RUN_TIMEOUT_MS = Number(process.env.BENCH_RUN_TIMEOUT_MS ?? "20000");
// browserarena navigates every provider to example.com and waits for
// `domcontentloaded`; keep the same default so timings are comparable.
const URL = process.env.BENCH_URL ?? "https://example.com";
// TCP port Chromium's CDP/DevTools endpoint listens on inside the sandbox,
// reached via the ingress proxy. Keep in sync with chromehost.
const CDP_PORT = Number(process.env.HIVER_BROWSER_PORT ?? "9223");
const sandboxConfig: hiver.SandboxConfig = {
  image: "browser",
};

// With BENCH_NAV_TIMING on (default) the goto stage also reads the browser's
// Navigation Timing (a post-navigation page.evaluate) so we can split the cost
// into DNS / connect / TTFB / response / DOM. That extra round-trip inflates the
// measured goto (it lands in `resid`); set BENCH_NAV_TIMING=0 to measure the bare
// page.goto and compare.
const NAV_TIMING = process.env.BENCH_NAV_TIMING !== "0";

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

interface Run {
  createMs: number; // provider "create session" (getOrCreateSandbox)
  connectMs: number; // chromium.connectOverCDP() attach to the resident browser
  pingMs: number; // a trivial page.evaluate() round-trip — the CDP transport floor
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

// connect / attach stage: chromium.connectOverCDP() to the resident browser over
// the relay's stable /cdp url (…/proxy/<port>/cdp, http→ws), retried until it
// answers. On the warm microvm path the browser is in the snapshot and connects
// on the first try; on the cold container path Chromium is still coming up, so we
// retry (≤ RUN_TIMEOUT_MS) until the relay is listening.
async function connectWithRetry(
  sandbox: hiver.Sandbox,
  label: string,
): Promise<Browser> {
  const wsEndpoint = sandbox.proxyUrl(CDP_PORT).replace(/^http/, "ws") + "/cdp";
  const start = performance.now();
  for (;;) {
    try {
      return await chromium.connectOverCDP(wsEndpoint, { timeout: 2_000 });
    } catch (e) {
      if (performance.now() - start > RUN_TIMEOUT_MS) {
        throw new Error(`${label} timed out after ${RUN_TIMEOUT_MS}ms: ${e}`);
      }
      await sleep(100);
    }
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

  // connect: chromium.connectOverCDP() to the resident browser over the stable
  // /cdp url. On the warm microvm path the browser is in the snapshot and connects
  // on the first try, so connectMs measures the warm attach; on the cold container
  // path it includes waiting for Chromium to come up.
  const t1 = performance.now();
  const browser = await connectWithRetry(sandbox, "connect");
  const connectMs = performance.now() - t1;

  let run: Run | null = null;
  try {
    // Reuse the resident browser's warm context + pre-opened page (a fresh CDP
    // target would spin up a new renderer); fall back if it came up empty.
    const context = browser.contexts()[0]!;
    const page = context.pages()[0]!;

    // ping: a trivial page.evaluate() with no browser work — the CDP transport
    // floor (one Runtime.evaluate round-trip through the proxy) so we can
    // attribute how much of the goto's overhead is transport vs browser.
    const tp = performance.now();
    // await page.evaluate(() => 1);
    const pingMs = performance.now() - tp;

    // goto: navigate the pre-opened page to URL and wait for `domcontentloaded` —
    // the "open page" stage and the best proxy for browser speed.
    const t2 = performance.now();
    await page.goto(URL, { waitUntil: "domcontentloaded", timeout: RUN_TIMEOUT_MS });
    const gotoMs = performance.now() - t2;

    // Optionally read the browser's Navigation Timing (an extra page.evaluate
    // round-trip, so it inflates `goto` into `resid` — see NAV_TIMING).
    let nav: NavTiming | undefined;
    if (NAV_TIMING) {
      nav =
        (await page.evaluate(() => {
          const e = performance.getEntriesByType(
            "navigation",
          )[0] as PerformanceNavigationTiming | undefined;
          return e
            ? {
                dns: e.domainLookupEnd - e.domainLookupStart,
                connect: e.connectEnd - e.connectStart,
                ttfb: e.responseStart - e.requestStart,
                response: e.responseEnd - e.responseStart,
                dom: e.domContentLoadedEventEnd - e.responseEnd,
                nav: e.domContentLoadedEventEnd - e.startTime,
              }
            : null;
        })) ?? undefined;
    }

    run = { createMs, connectMs, pingMs, gotoMs, releaseMs: 0, totalMs: 0, nav };
  } catch (e) {
    console.error(`  run ${i}: failed: ${e}`);
  } finally {
    // Disconnect this CDP client — does NOT terminate the resident browser, so
    // the sandbox stays warm until it's shut down in the cooldown.
    await browser.close().catch(() => {});
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
        await sandbox.shutdown();
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
// `ping` (the CDP transport floor — a bare page.evaluate round-trip) and
// `resid = xport − ping` (the rest:
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
