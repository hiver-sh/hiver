import type { Sandbox } from "@hiver.sh/client";

const POLL_INTERVAL_MS = 500;
const DEFAULT_TIMEOUT_MS = 60_000;

/**
 * Resolve once the sandbox's API server answers a ping, or reject if it stays
 * unreachable past the deadline. A freshly created or resuming sandbox can take
 * a moment before its server accepts requests; without this wait, the first
 * `/config` or `/directories` call races the boot and fails (leaving the file
 * explorer stuck on "No mounts configured."), and the event stream opens before
 * there's anything to stream. Polling until the server responds lets those
 * routes serve real data on the first load.
 *
 * Pass an `AbortSignal` to bail out early — e.g. when an SSE client disconnects
 * while we're still waiting — instead of polling out the full timeout.
 */
export async function waitForSandbox(
  sandbox: Sandbox,
  { timeoutMs = DEFAULT_TIMEOUT_MS, signal }: WaitForSandboxOptions = {},
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastErr: unknown;
  for (;;) {
    if (signal?.aborted) throw new Error("aborted");
    try {
      await sandbox.ping();
      return;
    } catch (err) {
      lastErr = err;
    }
    if (Date.now() >= deadline) break;
    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
  const detail = lastErr instanceof Error ? `: ${lastErr.message}` : "";
  throw new Error(
    `sandbox ${sandbox.id} did not become reachable within ${timeoutMs}ms${detail}`,
  );
}

export interface WaitForSandboxOptions {
  timeoutMs?: number;
  signal?: AbortSignal;
}
