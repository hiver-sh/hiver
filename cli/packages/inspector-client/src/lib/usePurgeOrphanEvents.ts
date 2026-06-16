import { useEffect } from "react";
import type { SandboxRef } from "@/types";
import { purgeOrphanEvents } from "@/lib/eventStore";

// How often to sweep IndexedDB for events belonging to vanished sandboxes.
// Kept below purgeOrphanEvents' grace period so eviction fires soon after it
// elapses rather than waiting for the next sandbox-list change.
const ORPHAN_PURGE_INTERVAL_MS = 10_000;

/**
 * Periodically evict stored events for sandboxes that no longer exist.
 * Uses the SSE-maintained sandbox list rather than polling the server.
 */
export function usePurgeOrphanEvents(sandboxes: SandboxRef[]): void {
  useEffect(() => {
    const ids = sandboxes.map((s) => s.id);
    void purgeOrphanEvents(ids);
    const interval = setInterval(() => void purgeOrphanEvents(ids), ORPHAN_PURGE_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [sandboxes]);
}
