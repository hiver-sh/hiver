import { Sandbox } from "@hiver.sh/client";
import type { SandboxEvent } from "@hiver.sh/client";
import { lastNestedEventId } from "./eventStore.js";

const ID_HEADER = "x-hiver-sandbox-id";
const KEY_HEADER = "x-hiver-sandbox-key";

export interface LinkedSandboxRelay {
  /**
   * Feed a primary-sandbox event in. On the first egress.response carrying
   * x-hiver-sandbox-id / x-hiver-sandbox-key headers for a new sandbox, its
   * event stream is opened and relayed.
   */
  relay(event: SandboxEvent): void;
  /**
   * Open a nested sandbox's stream directly, without waiting to see a linking
   * egress.response — used to resume sandboxes already known from storage.
   */
  openLinked(sandboxId: string, sandboxKey: string): void;
}

/**
 * Watches a primary sandbox's feed for links to other sandboxes and relays each
 * linked sandbox's events to `onEvent` (with sandbox_id / sandbox_key set to the
 * linked identity, so the client can route policy edits back to it). Each linked
 * stream resumes from the last event already persisted for that sandbox.
 *
 * Detection is recursive: each linked sandbox's own stream is re-scanned for
 * links, so a chain of nesting (a -> b -> c -> ...) is fully discovered.
 */
export function makeLinkedSandboxRelay(
  gatewayUrl: string,
  signal: AbortSignal,
  onEvent: (
    event: SandboxEvent & { sandbox_id: string; sandbox_key: string },
  ) => void,
  // Called once per newly-opened linked sandbox, so the caller can do more with
  // it than relay events — e.g. detect and attach its browser view.
  onLinked?: (sandbox: Sandbox) => void,
): LinkedSandboxRelay {
  const seen = new Set<string>();

  function openLinked(sandboxId: string, sandboxKey: string) {
    if (seen.has(sandboxId)) return;
    seen.add(sandboxId);

    const linked = new Sandbox(
      { id: sandboxId, key: sandboxKey },
      { gatewayUrl },
    );
    onLinked?.(linked);

    (async () => {
      try {
        // Resume this nested sandbox from the last event we've already persisted
        // for it (0 = from the beginning when nothing is stored yet), so a
        // reconnect doesn't refetch its whole history.
        for await (const e of linked.getEventsStream({
          signal,
          lastEventId: lastNestedEventId(sandboxId, sandboxKey) ?? 0,
        })) {
          onEvent({ ...e, sandbox_id: sandboxId, sandbox_key: sandboxKey });
          // Nesting is recursive: a nested sandbox can itself spawn another
          // (a -> b -> c). Re-scan its stream for linking egress.responses so
          // deeper sandboxes are discovered too. The shared `seen` set stops
          // re-opening ones already relayed (and breaks any link cycle).
          relay(e);
        }
      } catch {
        // stream aborted or sandbox gone
      }
    })();
  }

  function relay(event: SandboxEvent) {
    if (event.type !== "egress.response") return;
    const headers = (event as { headers?: Record<string, string> }).headers;
    if (!headers) return;
    const lower = Object.fromEntries(
      Object.entries(headers).map(([k, v]) => [k.toLowerCase(), v]),
    );
    const sandboxId = lower[ID_HEADER];
    const sandboxKey = lower[KEY_HEADER];
    if (!sandboxId || !sandboxKey) return;
    openLinked(sandboxId, sandboxKey);
  }

  return { relay, openLinked };
}
