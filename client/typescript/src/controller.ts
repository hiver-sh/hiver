import { ApiError, SandboxConfig, SandboxRef } from "./schemas";
import { Sandbox, SandboxError, toError } from "./sandbox";
import { parseSSE } from "./sse";

/** Gateway URL used when none is supplied via {@link GatewayOptions}. */
export const DEFAULT_GATEWAY_URL = "http://localhost:10000";

const SANDBOX_KEY_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

export interface GatewayOptions {
  /** Base URL of the gateway. Defaults to `http://localhost:10000`. */
  gatewayUrl?: string;
  /** Override the global fetch (e.g. for testing or custom transports). */
  fetch?: typeof fetch;
  /**
   * Timeout in milliseconds for controller operations, and the maximum time
   * `getOrCreateSandbox` waits for a new sandbox to become ready. Defaults to
   * 30s. Pass `0` to disable timeouts and skip the readiness wait.
   */
  timeoutMs?: number;
}

const DEFAULT_TIMEOUT_MS = 30_000;
const READINESS_POLL_INTERVAL_MS = 200;

/**
 * Create a sandbox, or fetch the existing one when `key` is already in use.
 * The key acts as an idempotency key: calling again with the same key returns
 * the same sandbox and leaves the supplied `config` unapplied. Resolves once
 * the sandbox is ready to accept requests.
 */
export async function getOrCreateSandbox(
  key: string,
  config: SandboxConfig = {},
  opts: GatewayOptions = {},
): Promise<Sandbox> {
  if (!SANDBOX_KEY_PATTERN.test(key)) {
    throw new Error(
      `getOrCreateSandbox: key ${JSON.stringify(key)} must match ${SANDBOX_KEY_PATTERN}`,
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
    return await provisionSandbox(key, validated, base, fetchImpl, timeout);
  } catch (err) {
    if (err instanceof SandboxError && err.status === 0 && timeout > 0) {
      return provisionSandbox(key, validated, base, fetchImpl, timeout);
    }
    throw err;
  }
}

async function provisionSandbox(
  key: string,
  config: SandboxConfig,
  base: string,
  fetchImpl: typeof fetch,
  timeout: number,
): Promise<Sandbox> {
  let res: Response;
  try {
    res = await fetchImpl(
      `${base}/controller/v1/sandboxes/${encodeURIComponent(key)}`,
      {
        method: "PUT",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(config),
        signal: timeout > 0 ? AbortSignal.timeout(timeout) : undefined,
      },
    );
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
  opts: GatewayOptions = {},
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
  return refs.map(
    (ref) => new Sandbox(ref, { gatewayUrl: base, fetch: fetchImpl }),
  );
}

/**
 * Permanently stop and remove a sandbox.
 */
export async function shutdown(
  sandbox: Sandbox,
  opts: GatewayOptions = {},
): Promise<void> {
  const base = (opts.gatewayUrl ?? DEFAULT_GATEWAY_URL).replace(/\/+$/, "");
  const fetchImpl = opts.fetch ?? fetch;
  const timeout = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const url = `${base}/controller/v1/shutdown/${encodeURIComponent(sandbox.key)}`;
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

/** Lifecycle transition a sandbox can go through. */
export type SandboxLifecycleStatus = "start" | "stop" | "die" | "destroy";
/** A lifecycle change observed for a single sandbox. */
export interface SandboxLifecycleEvent {
  /** Server-assigned unique identifier (uuid). */
  id: string;
  /** Caller-chosen key the sandbox was provisioned under. */
  key: string;
  /** Which lifecycle transition occurred. */
  status: SandboxLifecycleStatus;
}

/**
 * Watch lifecycle changes (start, stop, die, destroy) across all sandboxes as
 * they happen. Yields events until `signal` is aborted or the stream ends.
 */
export async function* watchSandboxEvents(
  opts: GatewayOptions = {},
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
