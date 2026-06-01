import {
  ApiError,
  ApplyResult,
  SandboxConfig,
  SandboxEvent,
  type SandboxRef,
} from "./schemas";
import { parseSSE } from "./sse";

const DEFAULT_TIMEOUT_MS = 5_000;

export interface SandboxOptions {
  /** Override the global fetch (e.g. for testing or proxying). */
  fetch?: typeof fetch;
}

export interface RequestOptions {
  /** Abort after this many milliseconds. Defaults to 5 000 for short operations. */
  timeoutMs?: number;
}

export interface ExecOptions {
  cwd?: string;
  signal?: AbortSignal;
  timeoutMs?: number;
}

export interface ExecStreamOptions {
  cwd?: string;
  signal?: AbortSignal;
  timeoutMs?: number;
}

export interface EventsStreamOptions {
  /**
   * Initial cursor: skip past this id on the first connect. After that
   * the stream tracks the cursor itself — on a transient disconnect
   * it reconnects with the latest id observed, so no events are
   * missed across drops.
   */
  lastEventId?: number;
  /** Abort the stream from the caller's side. */
  signal?: AbortSignal;
  /** Max number of retries if the connection is lost. Defaults to `3`. */
  maxRetries?: number;
}

/**
 * A handle to a provisioned sandbox.
 */
export class Sandbox {
  readonly id: string;
  /** Base URL of the per-sandbox API server. */
  readonly apiServerUrl: string;
  /**
   * Host and port of the HTTP service the sandbox image exposes (the first
   * TCP port from its EXPOSE directive), e.g. `"localhost:32768"`.
   * `undefined` when the image declares no EXPOSE port.
   */
  readonly exposedEndpoint: string | undefined;

  readonly fetchImpl: typeof fetch;

  constructor(ref: SandboxRef, opts: SandboxOptions) {
    this.id = ref.id;
    this.apiServerUrl = ref.endpoint.replace(/\/+$/, "");
    this.exposedEndpoint = ref.exposed_endpoint;
    this.fetchImpl = opts.fetch ?? fetch;
  }

  /**
   * Reset the sandbox's TTL countdown. Bound as an arrow so
   * `setInterval(sandbox.ping, 10_000)` works without an explicit
   * `.bind(sandbox)`.
   */
  ping = async (opts?: RequestOptions): Promise<void> => {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/ping`, { signal });
    if (!res.ok) throw await toError(res, "ping");
  };

  /** Read the current `SandboxConfig`. */
  async getConfig(opts?: RequestOptions): Promise<SandboxConfig> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/config`, { signal });
    if (!res.ok) throw await toError(res, "getConfig");
    return SandboxConfig.parse(await res.json());
  }

  /**
   * Apply a desired `SandboxConfig`. Returns an `ApplyResult` whose
   * `applied` field reports whether the change was committed or
   * rolled back.
   */
  async applyConfig(config: SandboxConfig, opts?: RequestOptions): Promise<ApplyResult> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const validated = SandboxConfig.parse(config);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/config`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(validated),
      signal,
    });
    if (!res.ok) throw await toError(res, "applyConfig");
    return ApplyResult.parse(await res.json());
  }

  /**
   * Long-lived async iterator over `SandboxEvent`s.
   *
   * Auto-resumes across disconnects: if the underlying SSE connection
   * drops (server restart, transient network blip, etc.) the iterator
   * silently reopens it with the last id observed, so the consumer
   * never sees a gap. Reconnect uses exponential backoff up to 30s
   * and runs forever — terminate the stream with `opts.signal`.
   */
  async *getEventsStream(
    opts: EventsStreamOptions = {},
  ): AsyncGenerator<SandboxEvent, void, void> {
    let lastEventId = opts.lastEventId;
    let backoffMs = 200;

    const maxRetries = opts.maxRetries || 3;
    let retry = 0;
    while (!opts.signal?.aborted) {
      if (retry > maxRetries) {
        return;
      }
      try {
        for await (const event of this.openEventsStream(
          lastEventId,
          opts.signal,
        )) {
          lastEventId = event.id;
          backoffMs = 200;
          yield event;
        }
        // Server closed the stream cleanly (e.g. shutdown). Fall
        // through to the backoff + reconnect path; if it really is
        // gone, subsequent attempts will keep failing until the
        // caller aborts.
      } catch (err) {
        if (opts.signal?.aborted) return;
        if (isAbortError(err)) return;

        console.log("err", err);
      }

      await sleep(backoffMs, opts.signal).catch(() => {});
      backoffMs = Math.min(backoffMs * 2, 30_000);
      retry++;
    }
  }

  // openEventsStream is one connection attempt — opens /v1/events,
  // parses SSE, yields events. Used by getEventsStream's reconnect
  // loop; not part of the public API.
  private async *openEventsStream(
    lastEventId: number | undefined,
    signal?: AbortSignal,
  ): AsyncGenerator<SandboxEvent, void, void> {
    const url = new URL(`${this.apiServerUrl}/v1/events`);
    if (lastEventId !== undefined) {
      url.searchParams.set("lastEventId", String(lastEventId));
    }
    const ac = new AbortController();
    if (signal) {
      if (signal.aborted) ac.abort(signal.reason);
      else
        signal.addEventListener("abort", () => ac.abort(signal.reason), {
          once: true,
        });
    }
    const res = await this.fetchImpl(url, {
      headers: { accept: "text/event-stream" },
      signal: ac.signal,
    });
    if (!res.ok || !res.body) throw await toError(res, "events");
    for await (const frame of parseSSE(res.body, ac.signal)) {
      yield SandboxEvent.parse(JSON.parse(frame.data));
    }
  }

  /**
   * Run `command` inside the sandbox and return buffered stdout, stderr,
   * and exit code once the process finishes.
   */
  async exec(command: string, opts?: ExecOptions): Promise<ExecResult> {
    const body: Record<string, string> = { command };
    if (opts?.cwd !== undefined) body.cwd = opts.cwd;
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/exec`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
      signal: resolveSignal(opts),
    });
    if (!res.ok) throw await toError(res, "exec");
    return res.json() as Promise<ExecResult>;
  }

  /**
   * Run `command` inside the sandbox and stream output as an async
   * iterator of `ExecStreamEvent`s. The final event has `type: "exit"`.
   */
  async *execStream(
    command: string,
    opts?: ExecStreamOptions,
  ): AsyncGenerator<ExecStreamEvent, void, void> {
    const body: Record<string, string> = { command };
    if (opts?.cwd !== undefined) body.cwd = opts.cwd;
    const signal = resolveSignal(opts);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/exec-stream`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        accept: "text/event-stream",
      },
      body: JSON.stringify(body),
      signal,
    });
    if (!res.ok || !res.body) throw await toError(res, "execStream");
    for await (const frame of parseSSE(res.body, signal)) {
      yield JSON.parse(frame.data) as ExecStreamEvent;
    }
  }

  /**
   * List the immediate children of a directory under a sandbox mount.
   * `path` is the agent-visible absolute path (e.g. `/workspace`).
   */
  async listDirectory(
    path: string,
    opts?: RequestOptions,
  ): Promise<{ name: string; path: string; is_dir: boolean; size: number }[]> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const url = new URL(`${this.apiServerUrl}/v1/directories`);
    url.searchParams.set("path", path);
    const res = await this.fetchImpl(url, { signal });
    if (!res.ok) throw await toError(res, "listDirectory");
    const body = (await res.json()) as {
      entries: { name: string; path: string; is_dir: boolean; size: number }[];
    };
    return body.entries;
  }

  /**
   * Download a file from a sandbox mount. `path` is the agent-visible
   * absolute path (e.g. `/workspace/data.csv`). Returns the raw bytes.
   */
  async downloadFile(path: string, opts?: RequestOptions): Promise<Uint8Array> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const url = new URL(`${this.apiServerUrl}/v1/file`);
    url.searchParams.set("path", path);
    const res = await this.fetchImpl(url, { signal });
    if (!res.ok) throw await toError(res, "downloadFile");
    return new Uint8Array(await res.arrayBuffer());
  }

  /**
   * Upload `content` as a file to `destination` (which must equal one
   * of the configured `fs[].mount` paths). `filename` becomes the
   * basename written under `destination`. Returns the agent-visible
   * path and byte count the server reports.
   */
  async uploadFile(
    destination: string,
    filename: string,
    content: Blob | Uint8Array | ArrayBuffer | string,
    opts?: RequestOptions,
  ): Promise<{ path: string; bytes: number }> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const form = new FormData();
    form.append("destination", destination);
    form.append("file", toBlob(content), filename);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/file`, {
      method: "POST",
      body: form,
      signal,
    });
    if (!res.ok) throw await toError(res, "uploadFile");
    const body = (await res.json()) as { path: string; bytes: number };
    return body;
  }
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  exit_code: number;
}

export type ExecStreamEvent =
  | { type: "stdout"; text: string }
  | { type: "stderr"; text: string }
  | { type: "exit"; code: number };

// Merge timeoutMs and signal into a single AbortSignal. If both are
// provided, abort whichever fires first.
function resolveSignal(
  opts: { signal?: AbortSignal; timeoutMs?: number } | undefined,
): AbortSignal | undefined {
  const { signal, timeoutMs } = opts ?? {};
  if (timeoutMs === undefined) return signal;
  const timeout = AbortSignal.timeout(timeoutMs);
  if (!signal) return timeout;
  const ac = new AbortController();
  if (signal.aborted) { ac.abort(signal.reason); return ac.signal; }
  if (timeout.aborted) { ac.abort(timeout.reason); return ac.signal; }
  signal.addEventListener("abort", () => ac.abort(signal.reason), { once: true });
  timeout.addEventListener("abort", () => ac.abort(timeout.reason), { once: true });
  return ac.signal;
}

function isAbortError(err: unknown): boolean {
  return (
    (err instanceof DOMException && err.name === "AbortError") ||
    (err instanceof Error && err.name === "AbortError")
  );
}

// sleep is cancellable: resolves after `ms`, or rejects with the
// signal's reason as soon as `signal` aborts. The caller in the
// reconnect loop wraps it in `.catch()` so an abort during backoff
// just exits the loop on the next iteration check.
function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal!.reason);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

function toBlob(content: Blob | Uint8Array | ArrayBuffer | string): Blob {
  if (content instanceof Blob) return content;
  if (typeof content === "string") return new Blob([content]);
  // BlobPart's typing on `Uint8Array<ArrayBufferLike>` is narrower than
  // what every modern runtime accepts at runtime; the cast lets callers
  // hand us a plain Uint8Array without first copying its buffer.
  return new Blob([content as BlobPart]);
}

/**
 * SandboxError carries the upstream JSON `Error` payload when the
 * server returned one, so callers can switch on `err.code` /
 * `err.body.details` without re-parsing the response.
 */
export class SandboxError extends Error {
  readonly status: number;
  readonly operation: string;
  readonly body?: { error: string; details?: Record<string, unknown> };

  constructor(
    operation: string,
    status: number,
    message: string,
    body?: { error: string; details?: Record<string, unknown> },
  ) {
    super(`${operation}: ${message}`);
    this.name = "SandboxError";
    this.status = status;
    this.operation = operation;
    this.body = body;
  }
}

export async function toError(
  res: Response,
  operation: string,
): Promise<SandboxError> {
  const text = await res.text();
  let body: { error: string; details?: Record<string, unknown> } | undefined;
  try {
    body = ApiError.parse(JSON.parse(text));
  } catch {
    body = undefined;
  }
  const message = body?.error ?? text ?? res.statusText;
  return new SandboxError(operation, res.status, message, body);
}
