import { createContext, useContext, useSyncExternalStore } from "react";
import type { SandboxEvent } from "@/types";

// The event feed is a high-frequency stream: during live streaming or trace
// replay a new event can arrive many times a second. Holding it as React state
// in SandboxDetail made that component the re-render root for the whole panel —
// every appended event reconciled the toolbar, the filter popover, and all four
// mosaic tiles, so keeping any of them still required memoizing each one and
// stabilizing every prop (not scalable, and easily defeated by a stray inline
// callback).
//
// Instead the feed lives here as an external store. Components subscribe only if
// they actually read events (the timeline, the file tree, the event counter), so
// a streamed event re-renders exactly those and nothing else — SandboxDetail
// itself no longer re-renders on the stream at all. The trace player is already
// an external store; this is the same idea for the SSE-derived feed.
//
// Updates are coalesced to at most one per animation frame. During trace replay
// the SSE stream can emit many events back-to-back (every recorded event whose
// time is already in the past resolves its wait immediately), so notifying — and
// re-rendering the timeline — per event pegged the CPU. Buffering appends and
// publishing once per frame bounds re-renders to the display rate no matter how
// fast events arrive, while still feeling instant.
const scheduleFrame: (cb: () => void) => number =
  typeof requestAnimationFrame !== "undefined"
    ? (cb) => requestAnimationFrame(cb)
    : (cb) => setTimeout(cb, 16) as unknown as number;
const cancelFrame: (handle: number) => void =
  typeof cancelAnimationFrame !== "undefined"
    ? (handle) => cancelAnimationFrame(handle)
    : (handle) => clearTimeout(handle);

export class EventStore {
  // Replaced (not mutated) on each published batch so `getEvents()` returns a
  // fresh reference whenever the feed changes — that's what useSyncExternalStore
  // compares, and what downstream `useMemo(…, [events])` depend on.
  private _events: SandboxEvent[] = [];
  // Appends buffered since the last published frame; concatenated in one shot.
  private _pending: SandboxEvent[] = [];
  private listeners = new Set<() => void>();
  private flushHandle: number | null = null;

  getEvents = (): SandboxEvent[] => this._events;

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  };

  append(event: SandboxEvent): void {
    this._pending.push(event);
    if (this.flushHandle === null) {
      this.flushHandle = scheduleFrame(this.flush);
    }
  }

  reset(): void {
    this._pending = [];
    if (this.flushHandle !== null) {
      cancelFrame(this.flushHandle);
      this.flushHandle = null;
    }
    if (this._events.length === 0) return;
    this._events = [];
    this.emit();
  }

  private flush = (): void => {
    this.flushHandle = null;
    if (this._pending.length === 0) return;
    this._events = this._events.concat(this._pending);
    this._pending = [];
    this.emit();
  };

  private emit(): void {
    for (const fn of this.listeners) fn();
  }
}

const EventStoreContext = createContext<EventStore | null>(null);
export const EventStoreProvider = EventStoreContext.Provider;

export function useEventStore(): EventStore {
  const store = useContext(EventStoreContext);
  if (!store) {
    throw new Error("useEventStore must be used within an EventStoreProvider");
  }
  return store;
}

// Stable empty snapshot for SSR (the embed server-renders the client tree) and
// as the getServerSnapshot cache — returning a fresh [] each call would throw.
const EMPTY_EVENTS: SandboxEvent[] = [];

// Subscribe to the full feed. Re-renders the caller on every append.
export function useEvents(): SandboxEvent[] {
  const store = useEventStore();
  return useSyncExternalStore(
    store.subscribe,
    store.getEvents,
    () => EMPTY_EVENTS,
  );
}

// Subscribe to just the count — re-renders only when the length changes, so a
// tiny counter node can update without pulling the whole event array through.
export function useEventCount(): number {
  const store = useEventStore();
  return useSyncExternalStore(
    store.subscribe,
    () => store.getEvents().length,
    () => 0,
  );
}
