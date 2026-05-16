import {
  ApiError,
  ApplyResult,
  SandboxConfig,
  SandboxEvent,
  type SandboxRef,
} from "./schemas";
import { parseSSE } from "./sse";

export interface SandboxOptions {
  /** Override the global fetch (e.g. for testing or proxying). */
  fetch?: typeof fetch;
}

export interface EventsStreamOptions {
  /** Resume the stream after this event id (server replays everything later). */
  lastEventId?: number;
  /** Abort the stream from the caller's side. */
  signal?: AbortSignal;
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

  private readonly fetchImpl: typeof fetch;

  constructor(ref: SandboxRef, opts: SandboxOptions = {}) {
    this.id = ref.id;
    this.apiServerUrl = ref.endpoint.replace(/\/+$/, "");
    this.fetchImpl = opts.fetch ?? fetch;
  }

  /**
   * URL of the HTTP service the sandbox image exposes (the first TCP
   * port from its EXPOSE directive). Append paths to it to reach the
   * upstream — `${sandbox.getUrl()}/healthz`, etc.
   */
  getUrl(): string {
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
   * Long-lived async iterator over `SandboxEvent`s. The HTTP request
   * is opened lazily on first `next()` and closes when the consumer
   * stops iterating or `signal` aborts.
   */
  async *getEventsStream(
    opts: EventsStreamOptions = {},
  ): AsyncGenerator<SandboxEvent, void, void> {
    const url = new URL(`${this.apiServerUrl}/v1/events`);
    if (opts.lastEventId !== undefined) {
      url.searchParams.set("lastEventId", String(opts.lastEventId));
    }
    const res = await this.fetchImpl(url, {
      headers: { accept: "text/event-stream" },
      signal: opts.signal,
    });
    if (!res.ok || !res.body) throw await toError(res, "events");
    for await (const frame of parseSSE(res.body, opts.signal)) {
      yield SandboxEvent.parse(JSON.parse(frame.data));
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

async function toError(res: Response, operation: string): Promise<SandboxError> {
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
