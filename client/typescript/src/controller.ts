import { ApiError, SandboxConfig, SandboxRef } from "./schemas";
import { Sandbox, SandboxError, toError } from "./sandbox";

export const DEFAULT_CONTROLLER_URL = "http://localhost:9000";

const SANDBOX_ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

export interface ControllerOptions {
  /** Base URL of the control plane. Defaults to `http://localhost:9000`. */
  controllerUrl?: string;
  /** Override the global fetch (e.g. for testing or custom transports). */
  fetch?: typeof fetch;
}

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
  config: SandboxConfig,
  opts: ControllerOptions = {},
): Promise<Sandbox> {
  if (!SANDBOX_ID_PATTERN.test(id)) {
    throw new Error(
      `getOrCreateSandbox: id ${JSON.stringify(id)} must match ${SANDBOX_ID_PATTERN}`,
    );
  }
  const validated = SandboxConfig.parse(config);
  const base = (opts.controllerUrl ?? DEFAULT_CONTROLLER_URL).replace(/\/+$/, "");
  const fetchImpl = opts.fetch ?? fetch;

  let res: Response;
  try {
    res = await fetchImpl(`${base}/v1/sandboxes/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(validated),
    });
  } catch (err) {
    if (isConnectionRefused(err)) {
      throw new SandboxError(
        "getOrCreateSandbox",
        0,
        `controller is not reachable at ${base} (connection refused). Is it running?`,
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
  return new Sandbox(ref, { controllerUrl: base, fetch: fetchImpl });
}

/**
 * Stop the sandbox container and remove it.
 */
export async function shutdown(sandbox: Sandbox): Promise<void> {
  const url = `${sandbox.controllerUrl}/v1/shutdown/${encodeURIComponent(sandbox.id)}`;
  let res: Response;
  try {
    res = await sandbox.fetchImpl(url, { method: "POST" });
  } catch (err) {
    if (isConnectionRefused(err)) {
      throw new SandboxError(
        "shutdown",
        0,
        `controller is not reachable at ${sandbox.controllerUrl} (connection refused). Is it running?`,
      );
    }
    throw err;
  }
  if (res.status === 204) return;
  throw await toError(res, "shutdown");
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
