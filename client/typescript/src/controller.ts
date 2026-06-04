import { ApiError, SandboxConfig, SandboxRef } from "./schemas";
import { Sandbox, SandboxError, toError } from "./sandbox";
import { parseSSE } from "./sse";

export const DEFAULT_GATEWAY_URL = "http://localhost:10000";

const SANDBOX_ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

export interface ControllerOptions {
  /** Base URL of the gateway. Defaults to `http://localhost:10000`. */
  gatewayUrl?: string;
  /** Override the global fetch (e.g. for testing or custom transports). */
  fetch?: typeof fetch;
  /**
   * Timeout in milliseconds applied to every controller fetch operation and
   * to the readiness polling loop in `getOrCreateSandbox`. Defaults to 30s.
   * Pass `0` to disable timeouts and skip the readiness wait.
   */
  timeoutMs?: number;
}

const DEFAULT_TIMEOUT_MS = 30_000;
const READINESS_POLL_INTERVAL_MS = 200;

/**
 * Idempotent provision against `PUT /v1/sandboxes/{id}`. If a sandbox
 * with `id` already exists the controller returns it unchanged and
 * the supplied `config` is ignored; otherwise the controller creates
 * a new sandbox from `config`.
 *
 * `config` is validated against the SandboxConfig schema before the
 * request is sent — a bad config fails fast on the caller side
 * instead of producing a 400 from the controller.
 */
export async function getOrCreateSandbox(
  id: string,
  config: SandboxConfig = {},
  opts: ControllerOptions = {},
): Promise<Sandbox> {
  if (!SANDBOX_ID_PATTERN.test(id)) {
    throw new Error(
      `getOrCreateSandbox: id ${JSON.stringify(id)} must match ${SANDBOX_ID_PATTERN}`,
    );
  }
  const validated = SandboxConfig.parse({
    fs: [
      {
        backend: "local",
        mount: "/workspace",
        acls: [{ path: "/workspace/**", access: "rw" }],
      },
    ],
    egress: [{ host: "*", access: "allow" }],
    ...config,
  });
  const base = (opts.gatewayUrl ?? DEFAULT_GATEWAY_URL).replace(/\/+$/, "");
  const fetchImpl = opts.fetch ?? fetch;
  const timeout = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  try {
    return await provisionSandbox(id, validated, base, fetchImpl, timeout);
  } catch (err) {
    if (err instanceof SandboxError && err.status === 0 && timeout > 0) {
      return provisionSandbox(id, validated, base, fetchImpl, timeout);
    }
    throw err;
  }
}

async function provisionSandbox(
  id: string,
  config: SandboxConfig,
  base: string,
  fetchImpl: typeof fetch,
  timeout: number,
): Promise<Sandbox> {
  let res: Response;
  try {
    res = await fetchImpl(`${base}/controller/v1/sandboxes/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(config),
      signal: timeout > 0 ? AbortSignal.timeout(timeout) : undefined,
    });
  } catch (err) {
    if (isConnectionRefused(err)) {
      throw new SandboxError(
        "getOrCreateSandbox",
        0,
        `gateway is not reachable at ${base} (connection refused). Is it running?`,
      );
    }
    throw err;
  }
  if (res.status !== 200 && res.status !== 201) {
    const text = await res.text();
    let body: { error: string; details?: Record<string, unknown> } | undefined;
    try {
      body = ApiError.parse(JSON.parse(text));
    } catch {
      body = undefined;
    }
    throw new SandboxError(
      "getOrCreateSandbox",
      res.status,
      body?.error ?? text ?? res.statusText,
      body,
    );
  }
  const ref = SandboxRef.parse(await res.json());
  const sandbox = new Sandbox(ref, { gatewayUrl: base, fetch: fetchImpl });
  if (timeout > 0) await waitUntilReachable(sandbox, timeout);
  return sandbox;
}

// waitUntilReachable polls /v1/ping until it returns 200 or the
// deadline passes.
async function waitUntilReachable(
  sandbox: Sandbox,
  timeoutMs: number,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastErr: unknown;
  while (Date.now() < deadline) {
    try {
      await sandbox.ping();
      return;
    } catch (err) {
      lastErr = err;
    }
    await new Promise((r) => setTimeout(r, READINESS_POLL_INTERVAL_MS));
  }
  const detail = lastErr instanceof Error ? `: ${lastErr.message}` : "";
  throw new SandboxError(
    "getOrCreateSandbox",
    0,
    `sandbox ${sandbox.id} did not become reachable at ${sandbox.apiServerUrl} within ${timeoutMs}ms${detail}`,
  );
}

/**
 * List all currently running sandboxes.
 */
export async function listSandboxes(
  opts: ControllerOptions = {},
): Promise<Sandbox[]> {
  const base = (opts.gatewayUrl ?? DEFAULT_GATEWAY_URL).replace(/\/+$/, "");
  const fetchImpl = opts.fetch ?? fetch;
  const timeout = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  let res: Response;
  try {
    res = await fetchImpl(`${base}/controller/v1/sandboxes`, {
      signal: timeout > 0 ? AbortSignal.timeout(timeout) : undefined,
    });
  } catch (err) {
    if (isConnectionRefused(err)) {
      throw new SandboxError(
        "listSandboxes",
        0,
        `gateway is not reachable at ${base} (connection refused). Is it running?`,
      );
    }
    throw err;
  }
  if (res.status !== 200) {
    throw await toError(res, "listSandboxes");
  }
  const refs = ((await res.json()) as unknown[]).map((r) =>
    SandboxRef.parse(r),
  );
  return refs.map((ref) => new Sandbox(ref, { gatewayUrl: base, fetch: fetchImpl }));
}

/**
 * Stop the sandbox container and remove it.
 */
export async function shutdown(
  sandbox: Sandbox,
  opts: ControllerOptions = {},
): Promise<void> {
  const base = (opts.gatewayUrl ?? DEFAULT_GATEWAY_URL).replace(/\/+$/, "");
  const fetchImpl = opts.fetch ?? fetch;
  const timeout = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const url = `${base}/controller/v1/shutdown/${encodeURIComponent(sandbox.id)}`;
  let res: Response;
  try {
    res = await fetchImpl(url, {
      method: "POST",
      signal: timeout > 0 ? AbortSignal.timeout(timeout) : undefined,
    });
  } catch (err) {
    if (isConnectionRefused(err)) {
      throw new SandboxError(
        "shutdown",
        0,
        `gateway is not reachable at ${base} (connection refused). Is it running?`,
      );
    }
    throw err;
  }
  if (res.status === 204) return;
  throw await toError(res, "shutdown");
}

export type SandboxLifecycleStatus = "start" | "stop" | "die" | "destroy";
export interface SandboxLifecycleEvent {
  id: string;
  status: SandboxLifecycleStatus;
}

/**
 * Stream sandbox lifecycle events via SSE from `GET /v1/sandboxes/events`.
 * Yields events until the signal is aborted or the server closes the stream.
 */
export async function* watchSandboxEvents(
  opts: ControllerOptions = {},
  signal?: AbortSignal,
): AsyncGenerator<SandboxLifecycleEvent, void, void> {
  const base = (opts.gatewayUrl ?? DEFAULT_GATEWAY_URL).replace(/\/+$/, "");
  const fetchImpl = opts.fetch ?? fetch;

  let res: Response;
  try {
    res = await fetchImpl(`${base}/controller/v1/sandboxes/events`, {
      headers: { Accept: "text/event-stream" },
      signal,
    });
  } catch (err) {
    if (isConnectionRefused(err)) {
      throw new SandboxError(
        "watchSandboxEvents",
        0,
        `gateway is not reachable at ${base} (connection refused). Is it running?`,
      );
    }
    throw err;
  }

  if (!res.ok || !res.body) {
    throw new SandboxError("watchSandboxEvents", res.status, res.statusText);
  }

  for await (const frame of parseSSE(res.body, signal)) {
    try {
      yield JSON.parse(frame.data) as SandboxLifecycleEvent;
    } catch {
      // skip malformed frames
    }
  }
}

function isConnectionRefused(err: unknown): boolean {
  if (!(err instanceof Error)) return false;
  const cause = (err as { cause?: unknown }).cause;
  if (hasCode(cause, "ECONNREFUSED")) return true;
  if (cause instanceof AggregateError) {
    return cause.errors.some((e) => hasCode(e, "ECONNREFUSED"));
  }
  return false;
}

function hasCode(e: unknown, code: string): boolean {
  return (
    typeof e === "object" &&
    e !== null &&
    (e as { code?: unknown }).code === code
  );
}
