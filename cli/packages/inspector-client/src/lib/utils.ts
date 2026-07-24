import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function humanDuration(ms: number): string {
  if (ms < 1000) return `${ms.toFixed(1)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  if (ms < 3_600_000) return `${(ms / 60_000).toFixed(1)}m`;
  return `${(ms / 3_600_000).toFixed(1)}h`;
}

// HH:MM:SS.mmm for an event timestamp, rendered in UTC to match the raw `Z`
// event stream (and `hiver events` output) — a chunk row and its event-feed row
// must never disagree by the viewer's UTC offset. Stream messages arrive
// milliseconds apart, so second resolution would collapse them into one value.
// Returns "" for a timestamp the runtime can't parse rather than "Invalid Date".
export function formatWallClock(timestamp: string): string {
  const d = new Date(timestamp);
  if (Number.isNaN(d.getTime())) return "";
  return d.toISOString().slice(11, 23);
}
