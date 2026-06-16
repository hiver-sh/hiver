import { useEffect } from "react";
import type { Dispatch, SetStateAction } from "react";
import type { SandboxRef } from "@/types";
import { clearEvents } from "@/lib/eventStore";
import { useTransport } from "@/lib/transport";
import { useUserPreferences } from "@/lib/userPreferences";

/**
 * Subscribe to the controller's sandbox lifecycle SSE stream and keep the
 * sandbox list in sync. On `destroy` the sandbox's persisted file-explorer
 * state and stored events are dropped immediately (the periodic purge in
 * usePurgeOrphanEvents is the backstop for sandboxes destroyed while the
 * inspector wasn't watching).
 *
 * This is one long-lived SSE per tab; affordable now that the per-sandbox
 * event feed and terminal share a single connection (see SandboxDetail's
 * `/stream`), so an open sandbox view holds 2 connections (lifecycle + stream)
 * rather than 3 — comfortably under the browser's ~6-per-origin HTTP/1.1 cap.
 */
export function useSandboxLifecycleEvents(
  serverUrl: string,
  setSandboxes: Dispatch<SetStateAction<SandboxRef[]>>,
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

      es.onmessage = (e) => {
        try {
          const event = JSON.parse(e.data) as {
            id: string;
            key: string;
            status: string;
          };
          if (event.status === "destroy") {
            forgetSandbox(event.key);
            void clearEvents(event.id);
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
              case "die":
                return prev.map((s) =>
                  s.key === event.key
                    ? { ...s, status: event.status as SandboxRef["status"] }
                    : s,
                );
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
  }, [serverUrl, gatewayUrl, transport, forgetSandbox, setSandboxes]);
}
