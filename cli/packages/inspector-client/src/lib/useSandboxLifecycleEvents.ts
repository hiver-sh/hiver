import { useEffect } from "react";
import type { Dispatch, SetStateAction } from "react";
import type { SandboxRef } from "@/types";
import { useTransport } from "@/lib/transport";
import { useUserPreferences } from "@/lib/userPreferences";

/**
 * Subscribe to the controller's sandbox lifecycle SSE stream and keep the
 * sandbox list in sync. `die` and `destroy` are both terminal — the sandbox's
 * container has exited for good (in single-sandbox mode sandboxd exits with it,
 * so the `die` never gets a following `destroy`) — so both drop the sandbox's
 * persisted file-explorer state and stored events and remove it from the list.
 * `stop` is non-terminal: the sandbox lingers in the list marked stopped.
 *
 * This is one long-lived SSE per tab; affordable now that the per-sandbox
 * event feed and terminal share a single connection (see SandboxDetail's
 * `/stream`), so an open sandbox view holds 2 connections (lifecycle + stream)
 * rather than 3 — comfortably under the browser's ~6-per-origin HTTP/1.1 cap.
 *
 * The stream carries deltas, not snapshots: any create/destroy that happens
 * while the connection is down (e.g. a gateway watch expiry, see `onerror`
 * below) would otherwise be lost. To keep the list authoritative without ever
 * polling, `onResync` fires on every (re)connect so the caller can pull a fresh
 * full snapshot; the live deltas then keep it current until the next drop.
 */
export function useSandboxLifecycleEvents(
  serverUrl: string,
  setSandboxes: Dispatch<SetStateAction<SandboxRef[]>>,
  onResync?: () => void,
): void {
  const { transport, gatewayUrl } = useTransport();
  const { forgetSandbox } = useUserPreferences();
  useEffect(() => {
    let closed = false;
    let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
    let es: ReturnType<typeof transport.openEventSource>;

    const connect = () => {
      const url = new URL(`${serverUrl}/api/sandboxes/events`);
      es = transport.openEventSource(url);

      // Every (re)connect re-syncs the full list from server truth, so any
      // create/destroy missed while the stream was down is reconciled. This
      // fires on the first connect too (harmless: it just reconfirms the list
      // the initial fetch already loaded). No polling — driven purely by the
      // stream opening.
      es.onopen = () => {
        if (!closed) onResync?.();
      };

      es.onmessage = (e) => {
        try {
          const event = JSON.parse(e.data) as {
            id: string;
            key: string;
            status: string;
          };
          // `die` is terminal like `destroy`: drop persisted state + stored
          // events. Events live server-side now, so clear them through the API.
          if (event.status === "destroy" || event.status === "die") {
            forgetSandbox(event.key);
            const url = new URL(
              `${serverUrl}/api/sandboxes/${encodeURIComponent(event.id)}/${encodeURIComponent(event.key)}/events`,
            );
            void transport.fetch(url, { method: "DELETE" }).catch(() => {});
          }
          setSandboxes((prev) => {
            switch (event.status) {
              case "start":
                return prev.find((s) => s.key === event.key)
                  ? prev.map((s) =>
                      s.key === event.key ? { ...s, status: "start" as const } : s,
                    )
                  : [...prev, { id: event.id, key: event.key, status: "start" as const }];
              case "stop":
                return prev.map((s) =>
                  s.key === event.key ? { ...s, status: "stop" as const } : s,
                );
              // Both terminal: the container is gone, so remove it from the list.
              case "die":
              case "destroy":
                return prev.filter((s) => s.key !== event.key);
              default:
                return prev;
            }
          });
        } catch {
          // ignore malformed frames
        }
      };

      // Reconnect when the stream closes (e.g. k8s watch expiry terminates the
      // SSE from the gateway side). FetchEventSource doesn't auto-reconnect the
      // way the native EventSource API does, so we do it manually.
      es.onerror = () => {
        if (closed) return;
        es.close();
        reconnectTimer = setTimeout(connect, 2_000);
      };
    };

    connect();

    return () => {
      closed = true;
      clearTimeout(reconnectTimer);
      es.close();
    };
    // gatewayUrl is included so the stream reconnects when the upstream
    // gateway changes (the transport stamps it as a request header).
  }, [serverUrl, gatewayUrl, transport, forgetSandbox, setSandboxes, onResync]);
}
