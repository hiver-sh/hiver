import {
  ApiError,
  ApplyResult,
  SandboxConfig,
  SandboxEvent,
  type SandboxRef,
} from "./schemas";
import { parseSSE } from "./sse";

const SANDBOX_FETCH_TIMEOUT_MS = 3_000;

export interface SandboxOptions {
  /**
   * Base URL of the controller that produced this handle. Stored so
   * controller-side operations (e.g. `hive.shutdown`) can reach back
   * without the caller having to remember it.
   */
  controllerUrl: string;
  /** Override the global fetch (e.g. for testing or proxying). */
  fetch?: typeof fetch;
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
 * A handle to a provisioned sandbox. Returned by `getOrCreateSandbox`;
 * not constructed directly by callers.
 *
 * The handle holds the per-sandbox API base URL and exposes the
 * operations against the endpoints described in `api/sandbox_server.yaml`:
 * config, ping, events, and the reverse proxy to the sandboxed
 * service.
 */
export class Sandbox {
  readonly id: string;
  /** Base URL of the per-sandbox API server (no trailing slash). */
  readonly apiServerUrl: string;
  /** Base URL of the controller that created this sandbox (no trailing slash). */
  readonly controllerUrl: string;

  readonly fetchImpl: typeof fetch;

  constructor(ref: SandboxRef, opts: SandboxOptions) {
    this.id = ref.id;
    this.apiServerUrl = ref.endpoint.replace(/\/+$/, "");
    this.controllerUrl = opts.controllerUrl.replace(/\/+$/, "");
    const baseFetch = opts.fetch ?? fetch;

    this.fetchImpl = (input, init) => {
      const signal = init?.signal ?? AbortSignal.timeout(SANDBOX_FETCH_TIMEOUT_MS);
      return baseFetch(input, { ...init, signal });
    };
  }

  /**
   * URL of the HTTP service the sandbox image exposes (the first TCP
   * port from its EXPOSE directive). Append paths to it to reach the
   * upstream — `${sandbox.url}/healthz`, etc.
   */
  get url(): string {
    return `${this.apiServerUrl}/v1/sandbox`;
  }

  /**
   * Reset the sandbox's TTL countdown. Bound as an arrow so
   * `setInterval(sandbox.ping, 10_000)` works without an explicit
   * `.bind(sandbox)`.
   */
  ping = async (): Promise<void> => {
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/ping`);
    if (!res.ok) throw await toError(res, "ping");
  };

  /** Read the current `SandboxConfig`. */
  async getConfig(): Promise<SandboxConfig> {
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/config`);
    if (!res.ok) throw await toError(res, "getConfig");
    return SandboxConfig.parse(await res.json());
  }

  /**
   * Apply a desired `SandboxConfig`. Returns an `ApplyResult` whose
   * `applied` field reports whether the change was committed or
   * rolled back.
   */
  async applyConfig(config: SandboxConfig): Promise<ApplyResult> {
    const validated = SandboxConfig.parse(config);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/config`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(validated),
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
        for await (const event of this.openEventsStream(lastEventId, opts.signal)) {
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
    // Timeout pattern for SSE: the first-event window matches the
    // per-request timeout (sandbox should be reachable and have at
    // least one event in the ring/about to publish within that
    // budget). After the first frame the stream is known healthy
    // and the timeout is cleared — the connection then stays open
    // for as long as the server keeps writing.
    const ac = new AbortController();
    if (signal) {
      if (signal.aborted) ac.abort(signal.reason);
      else signal.addEventListener("abort", () => ac.abort(signal.reason), { once: true });
    }
    const firstEventTimeout = setTimeout(
      () => ac.abort(new Error(`events: no frame within ${SANDBOX_FETCH_TIMEOUT_MS}ms`)),
      SANDBOX_FETCH_TIMEOUT_MS,
    );
    try {
      const res = await this.fetchImpl(url, {
        headers: { accept: "text/event-stream" },
        signal: ac.signal,
      });
      if (!res.ok || !res.body) throw await toError(res, "events");
      for await (const frame of parseSSE(res.body, ac.signal)) {
        // First frame lands → the connection is alive; drop the
        // liveness guard so trailing silences (the server just isn't
        // emitting anything right now) don't terminate it.
        clearTimeout(firstEventTimeout);
        yield SandboxEvent.parse(JSON.parse(frame.data));
      }
    } finally {
      clearTimeout(firstEventTimeout);
    }
  }

  /**
   * Download a file from a sandbox mount. `path` is the agent-visible
   * absolute path (e.g. `/workspace/data.csv`). Returns the raw bytes.
   */
  async downloadFile(path: string): Promise<Uint8Array> {
    const url = new URL(`${this.apiServerUrl}/v1/file`);
    url.searchParams.set("path", path);
    const res = await this.fetchImpl(url);
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
  ): Promise<{ path: string; bytes: number }> {
    const form = new FormData();
    form.append("destination", destination);
    form.append("file", toBlob(content), filename);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/file`, {
      method: "POST",
      body: form,
    });
    if (!res.ok) throw await toError(res, "uploadFile");
    const body = (await res.json()) as { path: string; bytes: number };
    return body;
  }
}

function isAbortError(err: unknown): boolean {
  return (
    err instanceof DOMException && err.name === "AbortError"
  ) || (
    err instanceof Error && err.name === "AbortError"
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

export async function toError(res: Response, operation: string): Promise<SandboxError> {
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
