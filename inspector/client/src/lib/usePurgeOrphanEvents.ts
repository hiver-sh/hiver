import { useEffect } from "react";
import type { SandboxRef } from "@/types";
import { purgeOrphanEvents } from "@/lib/eventStore";
import { useTransport } from "@/lib/transport";

// How often to sweep IndexedDB for events belonging to vanished sandboxes.
// Kept below purgeOrphanEvents' grace period so eviction fires soon after it
// elapses rather than waiting for the next sandbox-list change.
const ORPHAN_PURGE_INTERVAL_MS = 10_000;

/**
 * Periodically evict stored events for sandboxes that no longer exist.
 *
 * Reconciles against a fresh fetch of the authoritative list rather than the
 * SSE-maintained sandbox state, which can retain a sandbox whose `destroy`
 * event was missed and would otherwise keep its events alive forever.
 * purgeOrphanEvents applies its own grace period, so it must run repeatedly to
 * take effect.
 */
export function usePurgeOrphanEvents(serverUrl: string): void {
  const { transport } = useTransport();
  useEffect(() => {
    const purge = async () => {
      try {
        const res = await transport.fetch(new URL(`${serverUrl}/api/sandboxes`));
        if (!res.ok) return;
        const list = (await res.json()) as SandboxRef[];
        await purgeOrphanEvents(list.map((s) => s.id));
      } catch {
        // network/parse error — skip this tick
      }
    };
    void purge();
    const interval = setInterval(() => void purge(), ORPHAN_PURGE_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [serverUrl, transport]);
}
