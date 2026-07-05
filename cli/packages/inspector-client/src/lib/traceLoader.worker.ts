// Off-main-thread trace loading: fetches a recorded trace, decompresses it,
// and JSON-parses each line, posting parsed records back in batches. Moving
// this here keeps the ~700MB decompress + ~30k-line parse of a full trace off
// the main thread, which otherwise stalls rendering (CSS animations, input)
// for as long as it takes to churn through the file.
//
// Imported via `?worker&inline` in transport.tsx: Vite base64-embeds this
// entire script into the built library bundle at compile time, so `new
// Worker(...)` needs no separate file the host app's bundler has to resolve —
// unlike the Monaco workers (see monacoWorkers.ts), this file is on the
// published lib-entry.ts import graph, so it has to survive being re-bundled
// by whatever the consumer uses (Turbopack, webpack, ...).
import { Decompress as ZstdDecompress } from "fzstd";
import type { TraceRecord } from "./transport";

type TraceLine = { endpoint: string } & TraceRecord;

type LoadMessage = { type: "load"; tracePath: string };

export type TraceWorkerMessage =
  | { type: "records"; records: TraceLine[] }
  | { type: "done"; sawEvent: boolean }
  | { type: "error"; message: string };

// See the identically-named helper this replaced in transport.tsx for the
// rationale (a pull() that resolves without enqueueing anything stalls a
// ReadableStream forever, and fzstd legitimately consumes input without
// emitting when a compressed block spans an input-chunk boundary).
function zstdDecompressStream(
  input: ReadableStream<Uint8Array>,
): ReadableStream<Uint8Array> {
  const reader = input.getReader();
  let zstd: ZstdDecompress;
  let emitted = 0;
  let ended = false;
  return new ReadableStream<Uint8Array>({
    start(controller) {
      zstd = new ZstdDecompress((chunk, final) => {
        if (chunk.length > 0) {
          emitted++;
          controller.enqueue(chunk);
        }
        if (final) {
          ended = true;
          controller.close();
        }
      });
    },
    async pull() {
      const before = emitted;
      while (emitted === before && !ended) {
        const { done, value } = await reader.read();
        if (done) {
          zstd.push(new Uint8Array(0), true);
          return;
        }
        zstd.push(value);
      }
    },
    cancel(reason) {
      return reader.cancel(reason);
    },
  });
}

function parseLine(line: string): TraceLine | null {
  const t = line.trim();
  if (!t) return null;
  return JSON.parse(t) as TraceLine;
}

function isEventRecord(record: TraceLine): boolean {
  return (record.headers["content-type"] ?? "").includes("text/event-stream");
}

async function loadTrace(tracePath: string): Promise<void> {
  const res = await fetch(tracePath);
  if (!res.body) {
    postMessage({ type: "done", sawEvent: false } satisfies TraceWorkerMessage);
    return;
  }
  const body = tracePath.endsWith(".zst")
    ? zstdDecompressStream(res.body)
    : tracePath.endsWith(".gz")
      ? res.body.pipeThrough(new DecompressionStream("gzip"))
      : res.body;
  const reader = body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  let sawEvent = false;

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    const lines = buf.split("\n");
    buf = lines.pop() ?? ""; // keep the trailing partial line
    const records: TraceLine[] = [];
    for (const line of lines) {
      const record = parseLine(line);
      if (!record) continue;
      records.push(record);
      if (isEventRecord(record)) sawEvent = true;
    }
    // One batch per network chunk: a natural granularity (tens of records)
    // that keeps postMessage overhead low without adding artificial buffering.
    if (records.length > 0) {
      postMessage({ type: "records", records } satisfies TraceWorkerMessage);
    }
  }
  const last = parseLine(buf); // flush the final line (no trailing newline)
  if (last) {
    if (isEventRecord(last)) sawEvent = true;
    postMessage({
      type: "records",
      records: [last],
    } satisfies TraceWorkerMessage);
  }
  postMessage({ type: "done", sawEvent } satisfies TraceWorkerMessage);
}

self.onmessage = (e: MessageEvent<LoadMessage>) => {
  if (e.data.type !== "load") return;
  loadTrace(e.data.tracePath).catch((err: unknown) => {
    postMessage({
      type: "error",
      message: err instanceof Error ? err.message : String(err),
    } satisfies TraceWorkerMessage);
  });
};
