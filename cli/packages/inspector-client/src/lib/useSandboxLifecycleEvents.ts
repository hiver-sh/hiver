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
    const url = new URL(`${serverUrl}/api/sandboxes/events`);
    const es = transport.openEventSource(url);

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
                ? prev
                : [...prev, { id: event.id, key: event.key }];
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

    return () => es.close();
    // gatewayUrl is included so the stream reconnects when the upstream
    // gateway changes (the transport stamps it as a request header).
  }, [serverUrl, gatewayUrl, transport, forgetSandbox, setSandboxes]);
}
