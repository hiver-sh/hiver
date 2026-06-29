import { Sandbox } from "@hiver.sh/client";
import type { SandboxEvent } from "@hiver.sh/client";

const ID_HEADER = "x-hiver-sandbox-id";
const KEY_HEADER = "x-hiver-sandbox-key";

/**
 * Returns a per-event handler that watches for egress.response events carrying
 * x-hiver-sandbox-id / x-hiver-sandbox-key headers.  The first time a new
 * sandbox id is seen, a background getEventsStream is started for it and each
 * event is forwarded to `onEvent` with sandbox_id / sandbox_key set to the
 * header values, so the client can route policy edits back to that sandbox.
 */
export function makeLinkedSandboxRelay(
  gatewayUrl: string,
  signal: AbortSignal,
  onEvent: (
    event: SandboxEvent & { sandbox_id: string; sandbox_key: string },
  ) => void,
): (event: SandboxEvent) => void {
  const seen = new Set<string>();

  return function handleEvent(event: SandboxEvent) {
    if (event.type !== "egress.response") return;
    const headers = (event as { headers?: Record<string, string> }).headers;
    if (!headers) return;
    const lower = Object.fromEntries(
      Object.entries(headers).map(([k, v]) => [k.toLowerCase(), v]),
    );
    const sandboxId = lower[ID_HEADER];
    const sandboxKey = lower[KEY_HEADER];
    if (!sandboxId || !sandboxKey || seen.has(sandboxId)) return;
    seen.add(sandboxId);

    const linked = new Sandbox({ id: sandboxId, key: sandboxKey }, { gatewayUrl });

    (async () => {
      try {
        for await (const e of linked.getEventsStream({ signal })) {
          onEvent({ ...e, sandbox_id: sandboxId, sandbox_key: sandboxKey });
        }
      } catch {
        // stream aborted or sandbox gone
      }
    })();
  };
}
