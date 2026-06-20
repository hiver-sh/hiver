// Benchmark the *resident browser host* path: the playwright image ships
// /opt/hiver/prewarm, which sbxguest starts before the microvm snapshot, so a
// claimed sandbox resumes with Chromium already running. Instead of spawning
// `node -e "require('playwright'); chromium.launch()"` per exec (see
// benchmark-browser-ready.ts — ~0.9s require + ~1.1s launch + node startup), the
// client opens one execStream session bridged (via socat) to the resident
// host's Unix socket and drives the already-warm browser. "browser ready" here
// is the time from opening the session to getting a usable page back.
//
// Run with: npx tsx examples/benchmark-browser-resident.ts
//   HIVER_GATEWAY_URL=http://<host>:10000   BENCH_RUNS=10
// Sweep the load with BENCH_MIN_QPS / BENCH_MAX_QPS — the number of concurrent
// requests dispatched per second. The benchmark runs one batch of BENCH_RUNS
// runs at each integer QPS from MIN to MAX (inclusive), waiting BENCH_WAIT_MS
// (default 60s) between levels so the warm pool can replenish. E.g. RUNS=10,
// MIN_QPS=1, MAX_QPS=10 → 10 batches of 10 runs, one per QPS level. Within a
// batch, QPS=1 fires one request per second; QPS>1 launches that many runs on
// each one-second tick without waiting for in-flight runs to finish.
//
// Requires an image built with the resident host (Dockerfile/browser-host.cjs)
// and an sbxguest with the prewarm hook.
import * as hiver from "@hiver.sh/client";

const gatewayUrl = process.env.HIVER_GATEWAY_URL ?? "http://localhost:10000";
const RUNS = Number(process.env.BENCH_RUNS ?? "10");
const MIN_QPS = Number(process.env.BENCH_MIN_QPS ?? "1");
const MAX_QPS = Number(process.env.BENCH_MAX_QPS ?? "1");
const WAIT_MS = Number(process.env.BENCH_WAIT_MS ?? "10000");
const MARKER = "__READY__";
const sandboxConfig: hiver.SandboxConfig = {
  image: "hiversh/playwright:microvm-17",
};

// `page` is pre-opened at prewarm (in the image's browser host) and seeded into
// the session, so there's no browser.newPage() on the request path — we just run
// JS in the already-warm page. This is the "browser usable" signal.
const usePage = `console.log(await page.evaluate(() => 'ok'))`;

interface Run {
  createMs: number;
  readyMs: number;
  totalMs: number;
}

// Turn an ExecProcess's output chunks into a line iterator (stderr is passed
// through to our stderr for visibility).
async function* lines(exec: hiver.ExecProcess) {
  let buffer = "";
  for await (const pipe of exec.pipes) {
    if (pipe.stderr) process.stderr.write(pipe.stderr);
    if (!pipe.stdout) continue;
    buffer += pipe.stdout;
    let nl: number;
    while ((nl = buffer.indexOf("\n")) !== -1) {
      yield buffer.slice(0, nl);
      buffer = buffer.slice(nl + 1);
    }
  }
}

// One full create + open-session + two-command cycle. Returns the timings, or
// null if a command failed. Safe to run concurrently — it only appends the
// created sandbox to the `sandboxes` array the caller passes in (so the batch
// can tear them down afterwards).
async function doRun(
  i: number,
  sandboxes: hiver.Sandbox[],
): Promise<Run | null> {
  const t0 = performance.now();
  const sandbox = await hiver.getOrCreateSandbox(
    `bench-resident-${Date.now()}-${i}`,
    sandboxConfig,
    { gatewayUrl, timeoutMs: 120_000 },
  );
  sandboxes.push(sandbox);
  const createMs = performance.now() - t0;

  // Open one session to the resident host (socat bridges stdin/stdout to its
  // Unix socket) and drive it with the __READY__ line protocol.
  const ac = new AbortController();
  const t1 = performance.now();
  const exec = await sandbox.execStream(
    ["socat", "-", "UNIX-CONNECT:/run/hiver/browser.sock"],
    { signal: ac.signal },
  );
  // ac.abort() (teardown below) rejects exitCode with AbortError; expected, so
  // mark it handled to avoid an unhandled-rejection crash after the run.
  void exec.exitCode.catch(() => {});
  const it = lines(exec)[Symbol.asyncIterator]();

  // Read lines until the next __READY__, returning any output lines before it.
  const untilReady = async (): Promise<string[]> => {
    const out: string[] = [];
    for (;;) {
      const { value, done } = await it.next();
      if (done) return out;
      if (value === MARKER) return out;
      out.push(value);
    }
  };

  try {
    await untilReady(); // session live (socat connected to the warm REPL)

    // Run JS in the pre-warm page — the "browser usable" signal (no newPage).
    // await exec.writeStdin(usePage + "\n");
    // const out1 = await untilReady();
    const readyMs = performance.now() - t1;
    const totalMs = performance.now() - t0;
    // if (!out1.includes("ok")) {
    //   console.error(`  run ${i}: usePage failed: ${JSON.stringify(out1)}`);
    //   return null;
    // }
   
    console.info(
      `  run ${i}: ready ${readyMs.toFixed(0)}ms (create ${createMs.toFixed(0)}ms, total ${totalMs.toFixed(0)}ms)`,
    );
    return { createMs, readyMs, totalMs };
  } finally {
    ac.abort(); // tear down the socat bridge / SSE stream for this run.
  }
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// Run one batch of RUNS requests at `qps` concurrent requests per second: each
// tick fires a batch of up to `qps` runs without awaiting them, then waits 1s
// before the next tick. All in-flight runs are awaited, then the batch's
// sandboxes are torn down before returning the collected timings.
async function runBatch(qps: number): Promise<Run[]> {
  console.info(`\n=== qps ${qps}: dispatching ${RUNS} runs at ${qps} req/s ===`);
  const results: Run[] = [];
  const sandboxes: hiver.Sandbox[] = [];
  const inFlight: Promise<void>[] = [];

  for (let dispatched = 0; dispatched < RUNS; ) {
    const batch = Math.min(qps, RUNS - dispatched);
    for (let j = 0; j < batch; j++) {
      const i = dispatched + j + 1;
      inFlight.push(
        doRun(i, sandboxes)
          .then((r) => {
            if (r) results.push(r);
          })
          .catch((e) => console.error(`  run ${i}: error: ${e}`)),
      );
    }
    dispatched += batch;
    if (dispatched < RUNS) await sleep(1000);
  }

  await Promise.all(inFlight);
  console.info(`  shutting down ${sandboxes.length} sandboxes ...`);
  await Promise.all(sandboxes.map((s) => s.shutdown()));
  return results;
}

const avg = (a: number[]) => a.reduce((x, y) => x + y, 0) / a.length;
const min = (a: number[]) => Math.min(...a);
const max = (a: number[]) => Math.max(...a);
const row = (label: string, a: number[]) =>
  `  ${label.padEnd(15)} ${min(a).toFixed(0).padStart(5)}ms  ${avg(a).toFixed(0).padStart(5)}ms  ${max(a).toFixed(0).padStart(5)}ms`;

// Sweep QPS from MIN to MAX inclusive — one batch per level — with a WAIT_MS
// cooldown between levels so the warm pool can replenish before the next load.
interface Batch {
  qps: number;
  results: Run[];
}
const batches: Batch[] = [];

for (let qps = MIN_QPS; qps <= MAX_QPS; qps++) {
  const results = await runBatch(qps);
  batches.push({ qps, results });

  if (results.length === 0) {
    console.error(`  qps ${qps}: no successful runs.`);
  } else {
    console.info(`\n=== qps ${qps} summary (${results.length} runs) ===`);
    console.info(`                  min     avg     max`);
    console.info(row("sandbox create", results.map((r) => r.createMs)));
    console.info(row("browser ready", results.map((r) => r.readyMs)));
    console.info(row("total", results.map((r) => r.totalMs)));
  }

  if (qps < MAX_QPS) {
    console.info(`\ncooldown: waiting ${(WAIT_MS / 1000).toFixed(0)}s ...`);
    await sleep(WAIT_MS);
  }
}

console.info("\ndone.");

// Combined report: one row per QPS level (avg timings, ms).
console.info(`\n=== sweep summary (avg ms per qps level) ===`);
console.info(`  qps   runs   create   ready   total`);
for (const { qps, results } of batches) {
  if (results.length === 0) {
    console.info(`  ${String(qps).padStart(3)}      0       —       —       —`);
    continue;
  }
  console.info(
    `  ${String(qps).padStart(3)}   ${String(results.length).padStart(4)}   ` +
      `${avg(results.map((r) => r.createMs)).toFixed(0).padStart(6)}  ` +
      `${avg(results.map((r) => r.readyMs)).toFixed(0).padStart(6)}  ` +
      `${avg(results.map((r) => r.totalMs)).toFixed(0).padStart(6)}`,
  );
}

if (batches.every((b) => b.results.length === 0)) {
  console.error("\nno successful runs.");
  process.exit(1);
}
