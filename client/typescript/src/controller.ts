import { ApiError, SandboxConfig, SandboxRef } from "./schemas";
import { Sandbox, SandboxError, toError } from "./sandbox";
import { parseSSE } from "./sse";

/** Gateway URL used when none is supplied and `HIVER_GATEWAY_URL` is unset. */
export const DEFAULT_GATEWAY_URL = "http://localhost:10000";

/** Env var that overrides {@link DEFAULT_GATEWAY_URL} when no explicit URL is given. */
export const GATEWAY_URL_ENV = "HIVER_GATEWAY_URL";

/**
 * Resolve the gateway base URL. An explicit `gatewayUrl` (e.g. from
 * {@link GatewayOptions}) always wins; otherwise the `HIVER_GATEWAY_URL` env var
 * is used, falling back to {@link DEFAULT_GATEWAY_URL}. The env lookup is guarded
 * so the client also works where `process` is undefined (e.g. the browser).
 */
export function resolveGatewayUrl(gatewayUrl?: string): string {
  const fromEnv =
    typeof process !== "undefined" ? process.env?.[GATEWAY_URL_ENV] : undefined;
  return gatewayUrl ?? fromEnv ?? DEFAULT_GATEWAY_URL;
}

const SANDBOX_KEY_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

/**
 * Logical image name the gateway routes on when `config.image` is unset. The
 * gateway matches the `x-hiver-image` header against its per-image clusters, so
 * every create must carry one; this is the fallback.
 */
export const DEFAULT_IMAGE_NAME = "agent-base";


export interface GatewayOptions {
  /**
   * Base URL of the gateway. When omitted, the `HIVER_GATEWAY_URL` env var is
   * used, else {@link DEFAULT_GATEWAY_URL} (`http://localhost:10000`).
   */
  gatewayUrl?: string;
  /** Override the global fetch (e.g. for testing or custom transports). */
  fetch?: typeof fetch;
  /**
   * Timeout in milliseconds for controller operations. Defaults to 60s. Pass
   * `0` to disable timeouts.
   */
  timeoutMs?: number;
}

const DEFAULT_TIMEOUT_MS = 60_000;

/**
 * Create a sandbox, or fetch the existing one when `key` is already in use.
 * The key acts as an idempotency key: calling again with the same key returns
 * the same sandbox and leaves the supplied `config` unapplied. Resolves once
 * the sandbox is ready to accept requests.
 */
/**
 * Apply the client-side config defaults every create carries: an unset `fs`
 * becomes the standard `/workspace` local mount and an unset `egress` opens all
 * hosts. Shared by {@link getOrCreateSandbox} and `allowSandbox` — a config
 * pinned into an egress override replaces the nested create's body verbatim, so
 * it must carry the same defaults a direct create would, or the sandbox comes
 * up without a workspace while a resumed VM snapshot still holds the 9p mount.
 */
export function sandboxConfigWithDefaults(
  config: SandboxConfig = {},
): SandboxConfig {
  // Drop keys whose value is null/undefined so an explicitly-null field (e.g.
  // `fs: null` from a serialized config where an unset optional became null)
  // falls back to the default below instead of clobbering it and failing
  // validation (`.optional()` accepts undefined, not null).
  const provided = Object.fromEntries(
    Object.entries(config).filter(([, v]) => v != null),
  );
  return SandboxConfig.parse({
    fs: [
      {
        backend: "local",
        mount: "/workspace",
        acls: [{ path: "/workspace/**", access: "rw" }],
      },
    ],
    egress: [{ host: "*", access: "allow" }],
    ...provided,
  });
}

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
  const validated = sandboxConfigWithDefaults(config);
  const gatewayBase = resolveGatewayUrl(opts.gatewayUrl).replace(/\/+$/, "");
  const fetchImpl = opts.fetch ?? fetch;
  const timeout = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  const imageName = config.image ?? DEFAULT_IMAGE_NAME;
  const base = gatewayBase;

  try {
    return await provisionSandbox(
      key,
      validated,
      base,
      imageName,
      fetchImpl,
      timeout,
    );
  } catch (err) {
    if (err instanceof SandboxError && err.status === 0 && timeout > 0) {
      return provisionSandbox(
        key,
        validated,
        base,
        imageName,
        fetchImpl,
        timeout,
      );
    }
    throw err;
  }
}

async function provisionSandbox(
  key: string,
  config: SandboxConfig,
  base: string,
  imageName: string,
  fetchImpl: typeof fetch,
  timeout: number,
): Promise<Sandbox> {
  let res: Response;
  try {
    res = await fetchImpl(
      `${base}/v1/sandboxes/${encodeURIComponent(key)}`,
      {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "x-hiver-image": imageName,
          // The gateway consistent-hashes the create onto a pack host by this
          // header (the image clusters' MAGLEV hash_policy), so every
          // get-or-create for a key lands on the same pod. The key is also in
          // the path, but Envoy hashes on the header.
          "x-hiver-key": key,
        },
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
  return new Sandbox(ref, { gatewayUrl: base, fetch: fetchImpl });
}

/**
 * List all currently running sandboxes.
 */
export async function listSandboxes(
  opts: GatewayOptions = {},
): Promise<Sandbox[]> {
  const base = resolveGatewayUrl(opts.gatewayUrl).replace(/\/+$/, "");
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
  // Tolerate a null/absent body (e.g. an empty list serialized as JSON null).
  const body = ((await res.json()) as unknown[] | null) ?? [];
  const refs = body.map((r) => SandboxRef.parse(r));
  return refs.map(
    (ref) => new Sandbox(ref, { gatewayUrl: base, fetch: fetchImpl }),
  );
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
  const base = resolveGatewayUrl(opts.gatewayUrl).replace(/\/+$/, "");
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
