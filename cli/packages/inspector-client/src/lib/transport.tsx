import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ReactNode } from "react";
import TraceLoaderWorker from "./traceLoader.worker?worker&inline";
import type { TraceWorkerMessage } from "./traceLoader.worker";
import { useUserPreferences } from "./userPreferences";
import { DEFAULT_GATEWAY_URL } from "@/types";

export type TraceRecord = {
  time: number; // ms from recording start
  payload: string;
  headers: Record<string, string>;
};

export type TraceData = Record<string, TraceRecord[]>;

export interface EventSourceLike {
  onopen: (() => void) | null;
  onmessage: ((event: { data: string }) => void) | null;
  onerror: (() => void) | null;
  close(): void;
}

export interface Transport {
  fetch(url: string | URL, init?: RequestInit): Promise<Response>;
  openEventSource(url: string | URL): EventSourceLike;
}

// Parse any URL (absolute or relative) into pathname + decoded param map.
// Relative URLs like "/api/foo?path=/bar" have raw (un-encoded) param values;
// we split on "&" and decode each side manually so they compare equal to the
// percent-encoded values the browser produces via URLSearchParams.
function parseUrlParts(url: string | URL): {
  pathname: string;
  params: Map<string, string>;
} {
  if (url instanceof URL) {
    return { pathname: url.pathname, params: new Map(url.searchParams) };
  }
  try {
    const u = new URL(url);
    return { pathname: u.pathname, params: new Map(u.searchParams) };
  } catch {
    // Relative URL — split manually
    const qIdx = url.indexOf("?");
    const pathname = qIdx === -1 ? url : url.slice(0, qIdx);
    const params = new Map<string, string>();
    if (qIdx !== -1) {
      for (const part of url.slice(qIdx + 1).split("&")) {
        const eq = part.indexOf("=");
        if (eq === -1) continue;
        try {
          params.set(
            decodeURIComponent(part.slice(0, eq)),
            decodeURIComponent(part.slice(eq + 1)),
          );
        } catch {
          /* skip malformed */
        }
      }
    }
    return { pathname, params };
  }
}

function buildIndex(trace: TraceData): Map<string, TraceRecord[]> {
  const index = new Map<string, TraceRecord[]>();
  for (const [key, records] of Object.entries(trace)) {
    const { pathname, params } = parseUrlParts(key);
    // Canonical key: pathname + sorted decoded params re-encoded consistently
    const sorted = [...params.entries()].sort(([a], [b]) => a.localeCompare(b));
    const qs = new URLSearchParams(sorted).toString();
    index.set(pathname + (qs ? `?${qs}` : ""), records);
  }
  return index;
}

function parseSseData(payload: string): string | null {
  for (const line of payload.split("\n")) {
    if (line.startsWith("data: ")) return line.slice(6);
  }
  return null;
}

class NativeEventSource implements EventSourceLike {
  private _es: EventSource;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;

  constructor(url: string | URL) {
    this._es = new EventSource(url.toString());
    this._es.onopen = () => this.onopen?.();
    this._es.onmessage = (e) => this.onmessage?.({ data: e.data });
    this._es.onerror = () => this.onerror?.();
  }

  close() {
    this._es.close();
  }
}

export const liveTransport: Transport = {
  fetch: (url, init) => globalThis.fetch(url, init),
  openEventSource: (url) => new NativeEventSource(url),
};

class FetchEventSource implements EventSourceLike {
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  private _abort = new AbortController();

  constructor(
    url: string | URL,
    fetchFn: (url: string | URL, init?: RequestInit) => Promise<Response>,
  ) {
    (async () => {
      let res: Response;
      try {
        res = await fetchFn(url, { signal: this._abort.signal });
      } catch {
        if (!this._abort.signal.aborted) this.onerror?.();
        return;
      }
      if (!res.ok || !res.body) {
        this.onerror?.();
        return;
      }
      this.onopen?.();
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      while (true) {
        let done: boolean;
        let value: Uint8Array | undefined;
        try {
          ({ done, value } = await reader.read());
        } catch {
          if (!this._abort.signal.aborted) this.onerror?.();
          return;
        }
        if (done) {
          this.onerror?.();
          return;
        }
        buf += dec.decode(value, { stream: true });
        const parts = buf.split("\n\n");
        buf = parts.pop() ?? "";
        for (const part of parts)
          for (const line of part.split("\n"))
            if (line.startsWith("data: "))
              this.onmessage?.({ data: line.slice(6) });
      }
    })();
  }

  close() {
    this._abort.abort();
  }
}

function createGatewayTransport(
  base: Transport,
  gatewayUrl: string,
): Transport {
  const gatewayFetch: Transport["fetch"] = (url, init) => {
    const headers = new Headers(init?.headers);
    headers.set("x-gateway-url", gatewayUrl);
    return base.fetch(url, { ...init, headers });
  };
  return {
    fetch: gatewayFetch,
    openEventSource: (url) => new FetchEventSource(url, gatewayFetch),
  };
}

export class TracePlayer {
  private _trace: TraceData;
  private _index: Map<string, TraceRecord[]>;
  private _speed: number;
  private _baseReplayMs = 0;
  private _baseWallMs: number;
  private _listeners = new Set<() => void>();
  private _loadComplete: boolean;
  private _recordWaiters = new Set<() => void>();
  // Playback transport state (see the playback-control section below).
  private _paused = false;
  private _epoch = 0;
  private _durationMs = 0;
  private _timeWaiters: { target: number; resolve: () => void }[] = [];
  private _tickHandle: ReturnType<typeof setTimeout> | null = null;
  // Replay time the pending tick is scheduled to fire at. Tracked so a newly
  // registered, sooner waiter can pull the tick earlier instead of being stuck
  // behind a stale, later one (e.g. a waiter left by a stream aborted on seek).
  private _tickTarget = Infinity;

  // `streaming: true` marks a player that starts (near-)empty and fills in as
  // the trace loads (the tracePath flow) — callers must invoke finishLoading()
  // when the load settles. Non-streaming players are complete on construction.
  constructor(trace: TraceData, speed = 1, streaming = false) {
    this._trace = trace;
    this._index = buildIndex(trace);
    this._speed = speed;
    this._baseWallMs = Date.now();
    this._loadComplete = !streaming;
    // Total replay length = the latest recorded record time. Grows as a
    // streaming trace fills in (see addRecord).
    for (const records of Object.values(trace))
      for (const r of records)
        if (r.time > this._durationMs) this._durationMs = r.time;
  }

  // Notified whenever records are added (e.g. while a trace streams in), so
  // consumers can re-query a player that started empty and filled in over time.
  subscribe(fn: () => void): () => void {
    this._listeners.add(fn);
    return () => this._listeners.delete(fn);
  }

  // True once the trace is fully loaded — no more records will be added.
  get loadComplete(): boolean {
    return this._loadComplete;
  }

  // Called when the streaming load settles (success OR failure), so consumers
  // blocked in waitForRecords stop waiting and treat the trace as final.
  finishLoading(): void {
    if (this._loadComplete) return;
    this._loadComplete = true;
    const waiters = [...this._recordWaiters];
    this._recordWaiters.clear();
    for (const w of waiters) w();
  }

  // Wakes waitForRecords callers at most once per synchronous batch of
  // addRecord calls (the loader worker delivers records in batches): each
  // wakeup makes waiters re-check state — e.g. a waiting fetch re-running
  // findEntries — so waking per record would rescan tens of thousands of times
  // over a large load for no benefit.
  private _wakeScheduled = false;
  private _wakeRecordWaiters(): void {
    if (this._wakeScheduled || this._recordWaiters.size === 0) return;
    this._wakeScheduled = true;
    queueMicrotask(() => {
      this._wakeScheduled = false;
      const waiters = [...this._recordWaiters];
      this._recordWaiters.clear();
      for (const w of waiters) w();
    });
  }

  // Resolves when new records arrive, the load completes, or `signal` aborts —
  // whichever comes first. Lets the trace transport wait out the window where a
  // recorded endpoint hasn't been loaded yet instead of failing the request.
  waitForRecords(signal?: AbortSignal): Promise<void> {
    if (this._loadComplete || signal?.aborted) return Promise.resolve();
    return new Promise((resolve) => {
      const done = () => {
        this._recordWaiters.delete(done);
        signal?.removeEventListener("abort", done);
        resolve();
      };
      this._recordWaiters.add(done);
      signal?.addEventListener("abort", done, { once: true });
    });
  }

  get speed() {
    return this._speed;
  }

  get paused(): boolean {
    return this._paused;
  }

  // Total replay length in replay-ms (latest recorded record time).
  get durationMs(): number {
    return this._durationMs;
  }

  // Bumped by a backward seek. Consumers key their feed reset on this so they
  // re-pump the recorded streams from the start (see SandboxDetail).
  get epoch(): number {
    return this._epoch;
  }

  get elapsedReplayMs(): number {
    // While paused the clock is frozen at the value captured when we paused.
    if (this._paused) return this._baseReplayMs;
    return this._baseReplayMs + (Date.now() - this._baseWallMs) * this._speed;
  }

  // Fold the elapsed replay time into the base so a following speed/pause change
  // doesn't retroactively rescale the time already played.
  private _rebase(): void {
    this._baseReplayMs = this.elapsedReplayMs;
    this._baseWallMs = Date.now();
  }

  setSpeed(newSpeed: number): void {
    this._rebase();
    this._speed = newSpeed;
    this._reschedule();
    this._emitState();
  }

  pause(): void {
    if (this._paused) return;
    this._rebase();
    this._paused = true;
    this._clearTick();
    this._emitState();
  }

  resume(): void {
    if (!this._paused) return;
    this._baseWallMs = Date.now();
    this._paused = false;
    this._reschedule();
    this._emitState();
  }

  // Jump the replay clock to `replayMs`. A forward jump fast-forwards the open
  // recorded streams in place: their pending waits fire as the clock passes
  // them (see _reschedule). A backward jump can't rewind an already-advanced
  // stream, so we bump `epoch` to signal consumers to reset their feed and
  // re-pump from the start. Returns true when such a re-pump is required.
  seek(replayMs: number): boolean {
    const target = Math.max(0, replayMs);
    const backward = target < this.elapsedReplayMs;
    this._baseReplayMs = target;
    this._baseWallMs = Date.now();
    if (backward) this._epoch++;
    this._reschedule();
    this._emitState();
    return backward;
  }

  // Resolves once the replay clock reaches `traceTimeMs`. Driven by a single
  // internal ticker (not a per-wait setTimeout) so pause/resume/seek/speed
  // changes re-evaluate every in-flight wait at once — a raw setTimeout would
  // bake in a stale delay that a later pause or speed change couldn't revoke.
  waitUntil(traceTimeMs: number): Promise<void> {
    if (this.elapsedReplayMs >= traceTimeMs) return Promise.resolve();
    return new Promise((resolve) => {
      this._timeWaiters.push({ target: traceTimeMs, resolve });
      this._scheduleTick();
    });
  }

  private _fireDueWaiters(): void {
    if (this._timeWaiters.length === 0) return;
    const now = this.elapsedReplayMs;
    const due: (() => void)[] = [];
    const still: { target: number; resolve: () => void }[] = [];
    for (const w of this._timeWaiters) {
      if (w.target <= now) due.push(w.resolve);
      else still.push(w);
    }
    this._timeWaiters = still;
    for (const r of due) r();
  }

  private _clearTick(): void {
    if (this._tickHandle !== null) {
      clearTimeout(this._tickHandle);
      this._tickHandle = null;
    }
    this._tickTarget = Infinity;
  }

  // Schedule a single wake-up for the soonest pending waiter, timed against the
  // current speed. No-op while paused, with no waiters, or at speed ≤ 0. If a
  // tick is already pending, only reschedule when the soonest waiter is now
  // EARLIER than what that tick targets — otherwise the existing tick already
  // covers it (its _fireDueWaiters sweeps every due waiter, not just one).
  private _scheduleTick(): void {
    if (this._paused || this._speed <= 0 || this._timeWaiters.length === 0)
      return;
    let soonest = Infinity;
    for (const w of this._timeWaiters)
      if (w.target < soonest) soonest = w.target;
    if (this._tickHandle !== null && this._tickTarget <= soonest) return;
    this._clearTick();
    this._tickTarget = soonest;
    const wallDelay = (soonest - this.elapsedReplayMs) / this._speed;
    this._tickHandle = setTimeout(() => {
      this._tickHandle = null;
      this._tickTarget = Infinity;
      this._fireDueWaiters();
      this._scheduleTick();
    }, Math.max(0, wallDelay));
  }

  // Re-evaluate all waits against the (just-changed) clock, then reschedule.
  private _reschedule(): void {
    this._clearTick();
    this._fireDueWaiters();
    this._scheduleTick();
  }

  private _emitState(): void {
    for (const fn of this._listeners) fn();
  }

  addRecord(endpoint: string, record: TraceRecord): void {
    let records = this._trace[endpoint];
    if (!records) {
      // First sighting of this endpoint: compute its canonical key once and
      // point the index at the array. Later records mutate the same array, so
      // the index entry stays valid without recomputing the key.
      records = this._trace[endpoint] = [];
      const { pathname, params } = parseUrlParts(endpoint);
      const sorted = [...params.entries()].sort(([a], [b]) =>
        a.localeCompare(b),
      );
      const qs = new URLSearchParams(sorted).toString();
      this._index.set(pathname + (qs ? `?${qs}` : ""), records);
    }
    records.push(record);
    if (record.time > this._durationMs) this._durationMs = record.time;
    this._wakeRecordWaiters();
    for (const fn of this._listeners) fn();
  }

  findEntries(url: string | URL): TraceRecord[] | null {
    const { pathname, params: clientParams } = parseUrlParts(url);

    // Build the canonical key the same way buildIndex does and try exact match first.
    const sorted = [...clientParams.entries()].sort(([a], [b]) =>
      a.localeCompare(b),
    );
    const qs = new URLSearchParams(sorted).toString();
    const canonicalKey = pathname + (qs ? `?${qs}` : "");
    if (this._index.has(canonicalKey)) {
      return this._index.get(canonicalKey)!;
    }

    // Connection-specific params that change every session and should not block
    // matching. sandboxUrl and controller are dynamic ports; the sandbox identity
    // is already encoded in the pathname. exposedBackend changes similarly.
    const IGNORE_PARAMS = new Set([
      "sessionId",
      "cols",
      "rows",
      "sandboxUrl",
      "controller",
      "exposedBackend",
      "lastEventId",
      "gateway",
    ]);

    let best: TraceRecord[] | null = null;
    let bestKey = "";

    for (const [traceKey, records] of Object.entries(this._trace)) {
      const { pathname: tracePath, params: traceParams } =
        parseUrlParts(traceKey);
      if (tracePath !== pathname) continue;

      // All trace params must match the client, except ignored ephemeral ones.
      let allMatch = true;
      for (const [name, value] of traceParams) {
        if (IGNORE_PARAMS.has(name)) continue;
        if (clientParams.get(name) !== value) {
          allMatch = false;
          break;
        }
      }
      if (!allMatch) continue;

      // Prefer the entry with more matching params (more specific).
      if (
        best === null ||
        traceParams.size > parseUrlParts(bestKey).params.size
      ) {
        best = records;
        bestKey = traceKey;
      }
    }
    return best;
  }
}

// A no-op EventSource used during trace playback when a request has no
// recorded entries. It never opens and never errors, so it can't trigger
// reconnect loops or any network activity.
class NoopEventSource implements EventSourceLike {
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  close() {}
}

export const noopTransport: Transport = {
  fetch: () =>
    Promise.resolve(
      new Response(null, { status: 503, statusText: "Network disabled" }),
    ),
  openEventSource: () => new NoopEventSource(),
};

class TraceEventSource implements EventSourceLike {
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  private _closed = false;

  // Resolves its entries from the player lazily (instead of taking a snapshot)
  // for the same reasons as TraceTransport.fetch: the endpoint may not have
  // loaded yet when the source is opened, and the live entries array keeps
  // growing while the trace streams in. Opened-then-empty stays silent, exactly
  // like the NoopEventSource it replaces in that case.
  constructor(url: string | URL, player: TracePlayer) {
    (async () => {
      await Promise.resolve(); // yield so caller can set onopen/onmessage
      let entries = player.findEntries(url);
      while ((!entries || entries.length === 0) && !player.loadComplete) {
        await player.waitForRecords();
        if (this._closed) return;
        entries = player.findEntries(url);
      }
      if (this._closed || !entries || entries.length === 0) return;
      this.onopen?.();

      let i = 0;
      while (true) {
        if (this._closed) return;
        if (i >= entries.length) {
          if (player.loadComplete) return;
          await player.waitForRecords();
          continue;
        }
        const entry = entries[i++];
        await player.waitUntil(entry.time);
        if (this._closed) return;
        const data = parseSseData(entry.payload);
        if (data !== null) this.onmessage?.({ data });
      }
    })();
  }

  close() {
    this._closed = true;
  }
}

export class TraceTransport implements Transport {
  constructor(private _player: TracePlayer) {}

  async fetch(url: string | URL, init?: RequestInit): Promise<Response> {
    if (init?.signal?.aborted) throw new DOMException("Aborted", "AbortError");

    const method = (init?.method ?? "GET").toUpperCase();
    if (method !== "GET" && method !== "HEAD") {
      return new Response(null, { status: 204 });
    }

    // While the trace is still loading, an endpoint with no records yet may
    // simply not have arrived from the loader worker — wait for records rather
    // than failing. (The consumer's fetch of the event stream races the worker's
    // first batch; answering 404 here killed playback outright now that the
    // transport identity is stable and nothing retries the request.) Once the
    // load settles, a missing endpoint is genuinely "not recorded".
    let entries = this._player.findEntries(url);
    while ((!entries || entries.length === 0) && !this._player.loadComplete) {
      await this._player.waitForRecords(init?.signal ?? undefined);
      if (init?.signal?.aborted) throw new DOMException("Aborted", "AbortError");
      entries = this._player.findEntries(url);
    }
    if (!entries || entries.length === 0) {
      // During trace playback we never hit the network — a request with no
      // recorded entry resolves to an empty "not recorded" response.
      return new Response(null, { status: 404, statusText: "Not recorded" });
    }

    const first = entries[0];
    const contentType = first.headers["content-type"] ?? "";

    if (contentType.includes("text/event-stream")) {
      const stream = this._buildSseStream(entries, init?.signal || undefined);
      return new Response(stream, { status: 200, headers: first.headers });
    }

    // Pick the most recent snapshot at or before the current replay time.
    // Falls back to the first entry if replay hasn't reached any entry yet.
    const elapsed = this._player.elapsedReplayMs;
    const atOrBefore = entries.filter((e) => e.time <= elapsed);
    const entry =
      atOrBefore.length > 0 ? atOrBefore[atOrBefore.length - 1] : first;

    await this._player.waitUntil(entry.time);
    if (init?.signal?.aborted) throw new DOMException("Aborted", "AbortError");
    return new Response(entry.payload, { status: 200, headers: entry.headers });
  }

  openEventSource(url: string | URL): EventSourceLike {
    // Entry resolution (including the not-loaded-yet wait and the no-recorded-
    // stream silent case) lives inside TraceEventSource.
    return new TraceEventSource(url, this._player);
  }

  private _buildSseStream(
    entries: TraceRecord[],
    signal?: AbortSignal,
  ): ReadableStream<Uint8Array> {
    const player = this._player;
    const encoder = new TextEncoder();

    return new ReadableStream<Uint8Array>({
      start(controller) {
        if (signal) {
          signal.addEventListener(
            "abort",
            () => controller.error(new DOMException("Aborted", "AbortError")),
            { once: true },
          );
        }

        (async () => {
          // Coalesce all entries sharing a timestamp into a single enqueue so
          // the consumer sees them in one read turn. The recorder can split an
          // atomic terminal update — e.g. a clear-screen (`ESC[2J`) immediately
          // followed by its full repaint — into separate frames at the same
          // millisecond. Delivered as two reads, a paint can land between them
          // and the cleared, blank buffer flashes on screen before the repaint.
          // Emitting them together makes the update atomic: the terminal never
          // renders the intermediate empty state.
          // `entries` is the live array the player appends into as the trace
          // streams in (see findEntries/addRecord), so a drained stream isn't
          // necessarily finished: while the load is still in flight, wait for
          // more records instead of closing. (Closing early silently truncated
          // playback whenever the feed played faster than the trace loaded —
          // previously masked by the transport-identity churn re-opening the
          // stream every 250ms.)
          let i = 0;
          while (true) {
            if (signal?.aborted) return;
            if (i >= entries.length) {
              if (player.loadComplete) break;
              await player.waitForRecords(signal);
              continue;
            }
            const t = entries[i].time;
            await player.waitUntil(t);
            if (signal?.aborted) return;
            let combined = "";
            while (i < entries.length && entries[i].time === t) {
              const p = entries[i].payload;
              combined += p.endsWith("\n\n") ? p : p + "\n\n";
              i++;
            }
            try {
              controller.enqueue(encoder.encode(combined));
            } catch {
              return; // controller already closed/errored
            }
          }
          try {
            controller.close();
          } catch {
            // already closed
          }
        })();
      },
    });
  }
}

export interface TransportContextValue {
  transport: Transport;
  player: TracePlayer | null;
  playbackSpeed: number;
  setPlaybackSpeed: (speed: number) => void;
  // Replay is paused (clock frozen). Toggled via togglePaused. Meaningful only
  // while `player` is non-null.
  paused: boolean;
  togglePaused: () => void;
  // Jump the replay clock to a position in replay-ms (0…player.durationMs).
  seek: (replayMs: number) => void;
  // Bumps on a backward seek — a change signal consumers include in their feed-
  // reset dependencies so they re-pump the recorded streams from the start.
  seekEpoch: number;
  // Bumps on EVERY seek (forward or backward). Fetch-based panels (e.g. the file
  // tree, which pulls /directories on demand rather than off the stream) key a
  // reload on this so they re-resolve to the snapshot at the seeked time.
  seekNonce: number;
  loadTraceFromData: (data: TraceData) => void;
  clearTrace: () => void;
  gatewayUrl: string;
  setGatewayUrl: (url: string) => void;
  // Called by consumers (SandboxDetail) after the first event has actually
  // been committed to the UI. Relays to TransportProvider's onFirstEvent
  // callback, at most once per trace load.
  notifyFirstEvent: () => void;
}

export const TransportContext = createContext<TransportContextValue>({
  transport: liveTransport,
  player: null,
  playbackSpeed: 1,
  setPlaybackSpeed: () => {},
  paused: false,
  togglePaused: () => {},
  seek: () => {},
  seekEpoch: 0,
  seekNonce: 0,
  loadTraceFromData: () => {},
  clearTrace: () => {},
  gatewayUrl: DEFAULT_GATEWAY_URL,
  setGatewayUrl: () => {},
  notifyFirstEvent: () => {},
});

export function useTransport(): TransportContextValue {
  return useContext(TransportContext);
}

export interface TransportProviderProps {
  children: ReactNode;
  tracePath?: string;
  traceData?: TraceData;
  speed?: number;
  // Fires once per trace load, when the first replayed event has actually
  // been committed to the UI (SandboxDetail reports it via the context's
  // notifyFirstEvent). Traces with no event records at all fire it at end
  // of load instead, so a host's loading indicator can't hang forever.
  onFirstEvent?: () => void;
}

export function TransportProvider({
  children,
  tracePath,
  traceData: initialTraceData,
  speed = 1,
  onFirstEvent,
}: TransportProviderProps) {
  const { enableNetworkRequests } = useUserPreferences();
  const [player, setPlayer] = useState<TracePlayer | null>(null);
  // Kept in a ref so an inline callback prop doesn't restart the trace-
  // loading effects on every render.
  const onFirstEventRef = useRef(onFirstEvent);
  onFirstEventRef.current = onFirstEvent;
  const firstEventFiredRef = useRef(false);
  const notifyFirstEvent = useCallback(() => {
    if (firstEventFiredRef.current) return;
    firstEventFiredRef.current = true;
    onFirstEventRef.current?.();
  }, []);
  const [playbackSpeed, setPlaybackSpeedState] = useState(speed);
  const [paused, setPaused] = useState(false);
  const [seekEpoch, setSeekEpoch] = useState(0);
  const [seekNonce, setSeekNonce] = useState(0);
  // A freshly loaded/cleared trace player is never paused.
  useEffect(() => {
    setPaused(false);
  }, [player]);
  const [gatewayUrl, setGatewayUrlState] = useState(() => {
    // The server injects the CLI-resolved gateway as a global on every page
    // load (from its GATEWAY_URL, set by `hiver inspect` to the gateway you
    // `hiver connect`-ed to). When it differs from the one we last adopted it's
    // a fresh signal from the CLI — it wins and becomes the new override, so it
    // supersedes any stale value saved by a previous session. When it's
    // unchanged, a user's in-UI override (saved to localStorage) takes
    // precedence so it survives reloads. Falls back to the built-in default.
    const injected = (window as { __HIVE_GATEWAY_URL__?: string })
      .__HIVE_GATEWAY_URL__;
    try {
      if (injected && injected !== localStorage.getItem("inspector:gatewayInjected")) {
        localStorage.setItem("inspector:gatewayInjected", injected);
        localStorage.setItem("inspector:gatewayUrl", injected);
        return injected;
      }
      const stored = localStorage.getItem("inspector:gatewayUrl");
      if (stored) return stored;
    } catch {
      /* localStorage unavailable */
    }
    return injected || DEFAULT_GATEWAY_URL;
  });

  const setGatewayUrl = useCallback((url: string) => {
    setGatewayUrlState(url);
    try {
      localStorage.setItem("inspector:gatewayUrl", url);
    } catch {
      /* ignore */
    }
  }, []);

  // Identity changes only on REAL transport swaps — the player being created or
  // cleared, the gateway changing, network mode toggling. That identity change
  // is a load-bearing signal: data-loading effects across the app key on
  // `transport` (e.g. FileExplorer's loadMounts), and the live→trace swap must
  // re-run them or panels mounted before the player exists stay empty against
  // the noop transport. Crucially it does NOT change while a trace streams in:
  // the TraceTransport reads live from the player and waits for records (see
  // waitForRecords), so nothing needs re-querying as the trace fills — the old
  // throttled `traceVersion` identity churn re-rendered every useTransport()
  // consumer ~4x/sec for the whole replay, clearing selections and popovers.
  const baseTransport = useMemo(
    () =>
      player
        ? new TraceTransport(player)
        : enableNetworkRequests
          ? liveTransport
          : noopTransport,
    [player, enableNetworkRequests],
  );

  const transport = useMemo(
    () => createGatewayTransport(baseTransport, gatewayUrl),
    [baseTransport, gatewayUrl],
  );

  const setPlaybackSpeed = useCallback(
    (speed: number) => {
      setPlaybackSpeedState(speed);
      player?.setSpeed(speed);
    },
    [player],
  );

  const togglePaused = useCallback(() => {
    if (!player) return;
    if (player.paused) player.resume();
    else player.pause();
    setPaused(player.paused);
  }, [player]);

  const seek = useCallback(
    (replayMs: number) => {
      if (!player) return;
      // player.seek returns true for a backward jump, which can't fast-forward
      // an open stream in place — bump seekEpoch so consumers re-pump the feed.
      const needsReplay = player.seek(replayMs);
      setPaused(player.paused);
      if (needsReplay) setSeekEpoch((e) => e + 1);
      // Every seek (either direction) re-resolves fetch-based snapshots.
      setSeekNonce((n) => n + 1);
    },
    [player],
  );

  const loadTraceFromData = useCallback(
    (data: TraceData) => {
      setPlayer(new TracePlayer(data, playbackSpeed));
    },
    [playbackSpeed],
  );

  const clearTrace = useCallback(() => {
    setPlayer(null);
    setPlaybackSpeedState(1);
  }, []);

  useEffect(() => {
    if (initialTraceData) {
      firstEventFiredRef.current = false;
      setPlayer(new TracePlayer(initialTraceData, speed));
    }
  }, [initialTraceData]);

  useEffect(() => {
    if (!tracePath) return;
    firstEventFiredRef.current = false;
    // `streaming: true`: the player starts empty and fills in below, so trace
    // reads wait for records instead of failing while the load is in flight.
    const player = new TracePlayer({}, speed, true);
    setPlayer(player);

    // Fetching, decompressing, and JSON-parsing a full trace (hundreds of MB,
    // tens of thousands of lines) runs in a Worker so it can't block the main
    // thread's rendering — see traceLoader.worker.ts. This effect just drains
    // parsed records into the player as they arrive.
    const worker = new TraceLoaderWorker();
    worker.onmessage = (e: MessageEvent<TraceWorkerMessage>) => {
      const msg = e.data;
      if (msg.type === "records") {
        for (const { endpoint, ...record } of msg.records) {
          player.addRecord(endpoint, record);
        }
      } else if (msg.type === "done") {
        player.finishLoading();
        // The first-event signal normally fires from SandboxDetail once the
        // timeline has committed an event. A trace with no event records will
        // never reach that, so release the signal at end of load instead.
        if (!msg.sawEvent) notifyFirstEvent();
      } else if (msg.type === "error") {
        // Settle the load even on failure so consumers waiting for records
        // (see TraceTransport.fetch / waitForRecords) resolve instead of
        // hanging on a trace that will never finish.
        player.finishLoading();
        console.error("Failed to load trace:", msg.message);
      }
    };
    // Resolved to absolute here, on the main thread, where `window.location`
    // is the real page origin. A blob-URL worker's own `self.location` is the
    // blob: URL it was constructed from, which a relative path can't resolve
    // against — fetch() inside the worker would throw trying to parse it.
    const absoluteTracePath = new URL(tracePath, window.location.href).href;
    worker.postMessage({ type: "load", tracePath: absoluteTracePath });

    // Terminating abandons the worker's in-flight fetch/decompress loop
    // immediately — no separate cancellation protocol needed. Settle the player
    // too so anything still waiting on records unblocks.
    return () => {
      worker.terminate();
      player.finishLoading();
    };
    // speed intentionally excluded: only seeds this load's initial playback
    // speed, doesn't need to restart the load if it changes afterward.
    // notifyFirstEvent is stable (useCallback, refs only) — safe to omit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tracePath]);

  // Memoized so the value identity is stable across traceVersion bumps (and any
  // other provider re-render) — otherwise every consumer re-renders on each bump
  // even though `transport` is now stable. All fields are stable references
  // (setters are useCallback, transport is memoized) except player/speed/gateway,
  // which change rarely.
  const contextValue = useMemo<TransportContextValue>(
    () => ({
      transport,
      player,
      playbackSpeed,
      setPlaybackSpeed,
      paused,
      togglePaused,
      seek,
      seekEpoch,
      seekNonce,
      loadTraceFromData,
      clearTrace,
      gatewayUrl,
      setGatewayUrl,
      notifyFirstEvent,
    }),
    [
      transport,
      player,
      playbackSpeed,
      setPlaybackSpeed,
      paused,
      togglePaused,
      seek,
      seekEpoch,
      seekNonce,
      loadTraceFromData,
      clearTrace,
      gatewayUrl,
      setGatewayUrl,
      notifyFirstEvent,
    ],
  );

  return (
    <TransportContext.Provider value={contextValue}>
      {children}
    </TransportContext.Provider>
  );
}
