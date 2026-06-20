// Benchmark cold-start time for a microvm sandbox.
// Creates a fresh sandbox, calls listDirectory("/"), and prints timing for each phase.
//
// Run with: npx tsx examples/benchmark-start-time.ts
//
// Override the gateway with HIVER_GATEWAY_URL=http://<host>:10000
import * as hiver from "@hiver.sh/client";

const gatewayUrl = process.env.HIVER_GATEWAY_URL ?? "http://localhost:10000";
const RUNS = Number(process.env.BENCH_RUNS ?? "3");

const sandboxConfig: hiver.SandboxConfig = {
  image: "hiversh/node:alpine",
};

interface Run {
  run: number;
  startMs: number; // time until sandbox is reachable
  execMs: number; // time for `ls /` once sandbox is up
  totalMs: number;
}

const results: Run[] = [];
const sandboxes: hiver.Sandbox[] = [];

for (let i = 1; i <= RUNS; i++) {
  console.info(`\n--- run ${i}/${RUNS} ---`);

  const t0 = performance.now();
  const sandbox = await hiver.getOrCreateSandbox(
    `bench-start-${Date.now()}-${i}`,
    sandboxConfig,
    { gatewayUrl, timeoutMs: 120_000 },
  );
  sandboxes.push(sandbox);
  const startMs = performance.now() - t0;
  console.info(`  sandbox ready:  ${startMs.toFixed(0)}ms`);

  const t1 = performance.now();
  const entries = await sandbox.listDirectory("/");
  const execMs = performance.now() - t1;
  const totalMs = performance.now() - t0;

  console.info(`  ls / (${entries.length} entries): ${execMs.toFixed(0)}ms`);
  console.info(`  total:          ${totalMs.toFixed(0)}ms`);

  results.push({ run: i, startMs, execMs, totalMs });
}

console.info("\n--- shutting down sandboxes ---");
await Promise.all(sandboxes.map((s) => s.shutdown()));
console.info("done.");

const avg = (arr: number[]) => arr.reduce((a, b) => a + b, 0) / arr.length;
const min = (arr: number[]) => Math.min(...arr);
const max = (arr: number[]) => Math.max(...arr);

const startTimes = results.map((r) => r.startMs);
const execTimes = results.map((r) => r.execMs);
const totalTimes = results.map((r) => r.totalMs);

console.info(`\n=== summary (${RUNS} runs) ===`);
console.info(`               min     avg     max`);
console.info(
  `  sandbox up   ${min(startTimes).toFixed(0).padStart(5)}ms  ${avg(startTimes).toFixed(0).padStart(5)}ms  ${max(startTimes).toFixed(0).padStart(5)}ms`,
);
console.info(
  `  listDir /    ${min(execTimes).toFixed(0).padStart(5)}ms  ${avg(execTimes).toFixed(0).padStart(5)}ms  ${max(execTimes).toFixed(0).padStart(5)}ms`,
);
console.info(
  `  total        ${min(totalTimes).toFixed(0).padStart(5)}ms  ${avg(totalTimes).toFixed(0).padStart(5)}ms  ${max(totalTimes).toFixed(0).padStart(5)}ms`,
);
