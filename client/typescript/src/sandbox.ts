import {
  ApiError,
  ApplyResult,
  SandboxConfig,
  SandboxEvent,
  SandboxInfo,
  type Snapshot,
  SnapshotResult,
  type SandboxRef,
} from "./schemas";
import { parseSSE } from "./sse";

const DEFAULT_TIMEOUT_MS = 5_000;

export interface SandboxOptions {
  /** Base URL of the gateway. */
  gatewayUrl: string;
  /** Override the global fetch (e.g. for testing or custom transports). */
  fetch?: typeof fetch;
}

export interface RequestOptions {
  /** Abort after this many milliseconds. Defaults to 5 000 for short operations. */
  timeoutMs?: number;
}

export interface ExecOptions {
  /** Working directory to run the command in. When omitted, the sandbox's working directory is used. */
  cwd?: string;
  /**
   * Environment variables for the command, merged on top of the sandbox
   * config's `env`. When omitted, the sandbox config environment is used.
   */
  env?: Record<string, string>;
  /** Abort the command from the caller's side. */
  signal?: AbortSignal;
  /** Abort after this many milliseconds. */
  timeoutMs?: number;
}

export interface ExecStreamOptions {
  /** Working directory to run the command in. When omitted, the sandbox's working directory is used. */
  cwd?: string;
  /**
   * Environment variables for the command, merged on top of the sandbox
   * config's `env`. When omitted, the sandbox config environment is used.
   */
  env?: Record<string, string>;
  /** Allocate a pseudo-TTY so interactive programs behave as they would in a terminal. */
  tty?: boolean;
  /** Abort the command from the caller's side. */
  signal?: AbortSignal;
  /** Abort after this many milliseconds. */
  timeoutMs?: number;
}

export interface EventsStreamOptions {
  /**
   * Resume after this event id, skipping anything already seen — useful to
   * pick up where a previous stream left off.
   */
  lastEventId?: number;
  /** Abort the stream from the caller's side. */
  signal?: AbortSignal;
  /** Max number of reconnect attempts after a dropped connection. Defaults to `3`. */
  maxRetries?: number;
  /**
   * When `false`, replay the buffered events and stop instead of tailing
   * live. Defaults to `true`.
   */
  follow?: boolean;
}

/**
 * A handle to a provisioned sandbox.
 */
export class Sandbox {
  /** Server-assigned unique identifier (uuid). */
  readonly id: string;
  /** Caller-chosen key the sandbox was provisioned under; routes requests. */
  readonly key: string;
  /** Base URL of the per-sandbox API server. */
  readonly apiServerUrl: string;

  /**
   * Returns the base proxy URL for a specific port inside the sandbox.
   * Append the path to get a full URL, e.g. `sandbox.proxyUrl(8080) + "/health"`.
   */
  readonly proxyUrl: (port: number | string) => string;

  /** The `fetch` implementation used for all requests to this sandbox. */
  readonly fetchImpl: typeof fetch;

  /** Build a handle from a sandbox reference. Prefer {@link getOrCreateSandbox} over constructing directly. */
  constructor(ref: SandboxRef, opts: SandboxOptions) {
    this.id = ref.id;
    this.key = ref.key;
    this.apiServerUrl = `${opts.gatewayUrl.replace(/\/+$/, "")}/sandbox/${encodeURIComponent(ref.id)}`;
    this.proxyUrl = (port) => `${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/proxy/${port}`;
    this.fetchImpl = opts.fetch ?? fetch;
  }

  /**
   * Keep the sandbox alive by resetting its TTL countdown. Bound as an arrow
   * so it can be passed straight to `setInterval(sandbox.ping, 10_000)`.
   */
  ping = async (opts?: RequestOptions): Promise<void> => {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/ping`, {
      signal,
    });
    if (!res.ok) throw await toError(res, "ping");
  };

  /**
   * Tear this sandbox down via `DELETE /v1/<key>`, cancelling its lifecycle.
   */
  async shutdown(opts?: RequestOptions): Promise<void> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const res = await this.fetchImpl(
      `${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) throw await toError(res, "shutdown");
  }

  /**
   * List the network ports the sandbox exposes. Reach each one via
   * {@link proxyUrl}.
   */
  async getPorts(opts?: RequestOptions): Promise<number[]> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/ports`, {
      signal,
    });
    if (!res.ok) throw await toError(res, "getPorts");
    return (await res.json()) as number[];
  }

  /**
   * Read internal runtime info about the sandbox — currently the isolation
   * mechanism in use, which is selected automatically from the image (a microvm
   * image ships a guest root filesystem) rather than configured.
   */
  async getInfo(opts?: RequestOptions): Promise<SandboxInfo> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/info`, {
      signal,
    });
    if (!res.ok) throw await toError(res, "getInfo");
    return SandboxInfo.parse(await res.json());
  }

  /** Read the current `SandboxConfig`. */
  async getConfig(opts?: RequestOptions): Promise<SandboxConfig> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/config`, {
      signal,
    });
    if (!res.ok) throw await toError(res, "getConfig");
    return SandboxConfig.parse(await res.json());
  }

  /**
   * Apply a new `SandboxConfig`. The change is all-or-nothing: the returned
   * `ApplyResult.applied` reports whether it was committed or rolled back,
   * and `changes` details what was added or removed.
   */
  async applyConfig(
    config: SandboxConfig,
    opts?: RequestOptions,
  ): Promise<ApplyResult> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const validated = SandboxConfig.parse(config);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/config`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(validated),
      signal,
    });
    if (!res.ok) throw await toError(res, "applyConfig");
    return ApplyResult.parse(await res.json());
  }

  /**
   * Capture a snapshot of the running sandbox now, without stopping it. The
   * request selects which parts to capture: `vm` (full microVM state, keyed for
   * a later resume; a no-op on container isolation) and/or `files` (the writable
   * filesystem). Each part is reported independently in the result.
   */
  async snapshot(
    request: Snapshot,
    opts?: RequestOptions,
  ): Promise<SnapshotResult> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/snapshot`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(request),
      signal,
    });
    if (!res.ok) throw await toError(res, "snapshot");
    return SnapshotResult.parse(await res.json());
  }

  /**
   * Stream the sandbox's activity events (egress, filesystem, exec, stdio,
   * resource usage) as they happen.
   *
   * Auto-resumes across transient disconnects without dropping events. Stop
   * the stream with `opts.signal`, or set `opts.follow: false` to replay the
   * buffered events and finish instead of tailing live.
   */
  async *getEventsStream(
    opts: EventsStreamOptions = {},
  ): AsyncGenerator<SandboxEvent, void, void> {
    let lastEventId = opts.lastEventId;
    let backoffMs = 200;
    const follow = opts.follow ?? true;

    const maxRetries = follow ? (opts.maxRetries ?? 3) : 0;
    let retry = 0;
    while (!opts.signal?.aborted) {
      if (retry > maxRetries) {
        return;
      }
      try {
        for await (const event of this.openEventsStream(
          lastEventId,
          opts.signal,
          follow,
        )) {
          lastEventId = event.id;
          backoffMs = 200;
          yield event;
        }
        if (!follow) return;
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
    follow = true,
  ): AsyncGenerator<SandboxEvent, void, void> {
    const url = new URL(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/events`);
    if (lastEventId !== undefined) {
      url.searchParams.set("lastEventId", String(lastEventId));
    }
    if (!follow) {
      url.searchParams.set("follow", "false");
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

    // When not following, close the connection after a short idle window.
    // The server sends the replay burst then goes silent; 500 ms of no
    // frames means the backlog is exhausted.
    let idleTimer: ReturnType<typeof setTimeout> | undefined;
    const armIdle = () => {
      clearTimeout(idleTimer);
      idleTimer = setTimeout(() => ac.abort(), 500);
    };
    if (!follow) armIdle();

    try {
      for await (const frame of parseSSE(res.body, ac.signal)) {
        if (!follow) armIdle();
        yield SandboxEvent.parse(JSON.parse(frame.data));
      }
    } catch (err) {
      if (!isAbortError(err)) throw err;
    } finally {
      clearTimeout(idleTimer);
      ac.abort();
    }
  }

  /**
   * Run `command` inside the sandbox and return buffered stdout, stderr,
   * and exit code once the process finishes.
   *
   * `command` may be a string (passed to a shell via `sh -c`) or a string
   * array (executed directly as argv, each element a literal argument with no
   * shell, word-splitting, or expansion).
   */
  async exec(
    command: string | string[],
    opts?: ExecOptions,
  ): Promise<ExecResult> {
    const body: Record<string, unknown> = { command };
    if (opts?.cwd !== undefined) body.cwd = opts.cwd;
    if (opts?.env !== undefined) body.env = opts.env;
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/exec`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
      signal: resolveSignal(opts),
    });
    if (!res.ok) throw await toError(res, "exec");
    return res.json() as Promise<ExecResult>;
  }

  /**
   * Run `command` inside the sandbox and return an `ExecProcess` handle for
   * interactive use: stream output via `exec.pipes`, send input via
   * `exec.writeStdin()`, and await the result via `exec.exitCode`.
   *
   * `command` may be a string (passed to a shell via `sh -c`) or a string
   * array (executed directly as argv, each element a literal argument with no
   * shell, word-splitting, or expansion).
   *
   * Pass an empty (or omitted) `command` to attach to the sandbox
   * entrypoint's terminal instead of running a new command — this requires
   * the sandbox to have been created with `tty: true`. The stream stays open
   * until the entrypoint exits or you disconnect.
   *
   * Resolves once the process is ready, so `writeStdin` is safe to call.
   */
  async execStream(
    command?: string | string[],
    opts?: ExecStreamOptions,
  ): Promise<ExecProcess> {
    const id = crypto.randomUUID();
    const streamUrl = `${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/exec-stream/${encodeURIComponent(id)}`;
    const stdinUrl = `${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/exec-stream/${encodeURIComponent(id)}/stdin`;

    const body: Record<string, unknown> = {};
    // A non-empty string or array runs a command; "" / [] / undefined attaches
    // to the entrypoint terminal, so omit the field in that case.
    if (command && command.length > 0) body.command = command;
    if (opts?.cwd !== undefined) body.cwd = opts.cwd;
    if (opts?.env !== undefined) body.env = opts.env;
    if (opts?.tty !== undefined) body.tty = opts.tty;
    const signal = resolveSignal(opts);

    // Await the response headers — the server stores the process before
    // sending 200, so once this resolves writeStdin is safe to call.
    const res = await this.fetchImpl(streamUrl, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        accept: "text/event-stream",
      },
      body: JSON.stringify(body),
      signal,
    });
    if (!res.ok || !res.body) throw await toError(res, "execStream");

    // Queue bridging the SSE reader goroutine → pipes async iterable.
    const queue: (ExecPipeEvent | null)[] = [];
    let notify: (() => void) | null = null;
    const push = (item: ExecPipeEvent | null) => {
      queue.push(item);
      notify?.();
      notify = null;
    };

    let resolveExit!: (code: number) => void;
    let rejectExit!: (err: unknown) => void;
    const exitCode = new Promise<number>((res, rej) => {
      resolveExit = res;
      rejectExit = rej;
    });

    const fetchImpl = this.fetchImpl;

    // Consume the SSE body in the background; the caller drives it via `pipes`.
    (async () => {
      for await (const frame of parseSSE(res.body!, signal)) {
        const event = JSON.parse(frame.data) as ExecStreamEvent;
        if (event.type === "stdout") push({ stdout: event.text });
        else if (event.type === "stderr") push({ stderr: event.text });
        else if (event.type === "exit") {
          resolveExit(event.code);
          push(null);
        }
      }
    })().catch((err) => {
      rejectExit(err);
      push(null);
    });

    async function* pipesGen(): AsyncGenerator<ExecPipeEvent> {
      while (true) {
        if (queue.length > 0) {
          const item = queue.shift()!;
          if (item === null) return;
          yield item;
        } else {
          await new Promise<void>((r) => {
            notify = r;
          });
        }
      }
    }

    const writeStdin = async (data: string): Promise<void> => {
      const res = await fetchImpl(stdinUrl, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ data }),
      });
      if (!res.ok) throw await toError(res, "execStreamStdin");
    };

    return new ExecProcess({
      id,
      pipes: { [Symbol.asyncIterator]: pipesGen },
      exitCode,
      writeStdin,
    });
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
    const url = new URL(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/directories`);
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
  async readFile(path: string, opts?: RequestOptions): Promise<Uint8Array> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const url = new URL(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/file`);
    url.searchParams.set("path", path);
    const res = await this.fetchImpl(url, { signal });
    if (!res.ok) throw await toError(res, "readFile");
    return new Uint8Array(await res.arrayBuffer());
  }

  /**
   * Upload `content` as a file to `destination` (which must equal one
   * of the configured `fs[].mount` paths). `filename` becomes the
   * basename written under `destination`. Returns the agent-visible
   * path and byte count the server reports.
   */
  async writeFile(
    destination: string,
    filename: string,
    content: Blob | Uint8Array | ArrayBuffer | string,
    opts?: RequestOptions,
  ): Promise<{ path: string; bytes: number }> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const form = new FormData();
    form.append("destination", destination);
    form.append("file", toBlob(content), filename);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/file`, {
      method: "POST",
      body: form,
      signal,
    });
    if (!res.ok) throw await toError(res, "writeFile");
    const body = (await res.json()) as { path: string; bytes: number };
    return body;
  }

  /**
   * Delete a file or empty directory at `path` inside a sandbox mount.
   * `path` is the agent-visible absolute path (e.g. `/workspace/data.csv`).
   */
  async deleteFile(path: string, opts?: RequestOptions): Promise<void> {
    const signal = AbortSignal.timeout(opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const url = new URL(`${this.apiServerUrl}/v1/${encodeURIComponent(this.key)}/file`);
    url.searchParams.set("path", path);
    const res = await this.fetchImpl(url, { method: "DELETE", signal });
    if (!res.ok) throw await toError(res, "deleteFile");
  }
}

export interface ExecResult {
  /** Everything the command wrote to stdout. */
  stdout: string;
  /** Everything the command wrote to stderr. */
  stderr: string;
  /** The command's process exit code. */
  exit_code: number;
}

export interface ExecPipeEvent {
  /** A chunk of stdout output. Present on stdout frames. */
  stdout?: string;
  /** A chunk of stderr output. Present on stderr frames. */
  stderr?: string;
}

export class ExecProcess {
  /** Unique id for this exec invocation. */
  readonly id: string;
  /** Async iterable of output chunks (stdout/stderr) emitted as the process runs. */
  readonly pipes: AsyncIterable<ExecPipeEvent>;
  /** Resolves with the process exit code once it finishes. */
  readonly exitCode: Promise<number>;
  private readonly _writeStdin: (data: string) => Promise<void>;

  constructor(opts: {
    id: string;
    pipes: AsyncIterable<ExecPipeEvent>;
    exitCode: Promise<number>;
    writeStdin: (data: string) => Promise<void>;
  }) {
    this.id = opts.id;
    this.pipes = opts.pipes;
    this.exitCode = opts.exitCode;
    this._writeStdin = opts.writeStdin;
  }

  /** Send `data` to the process's standard input. */
  writeStdin(data: string): Promise<void> {
    return this._writeStdin(data);
  }
}

/** A single frame from a streaming exec: an output chunk or the final exit code. */
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
  if (signal.aborted) {
    ac.abort(signal.reason);
    return ac.signal;
  }
  if (timeout.aborted) {
    ac.abort(timeout.reason);
    return ac.signal;
  }
  signal.addEventListener("abort", () => ac.abort(signal.reason), {
    once: true,
  });
  timeout.addEventListener("abort", () => ac.abort(timeout.reason), {
    once: true,
  });
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
  /** HTTP status from the failed response, or `0` if the request never reached the server. */
  readonly status: number;
  /** The client operation that failed (e.g. `"applyConfig"`). */
  readonly operation: string;
  /** Structured error payload from the server, when one was returned. */
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
