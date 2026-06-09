import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import type { ReactNode } from "react";
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

  constructor(trace: TraceData, speed = 1) {
    this._trace = trace;
    this._index = buildIndex(trace);
    this._speed = speed;
    this._baseWallMs = Date.now();
  }

  // Notified whenever records are added (e.g. while a trace streams in), so
  // consumers can re-query a player that started empty and filled in over time.
  subscribe(fn: () => void): () => void {
    this._listeners.add(fn);
    return () => this._listeners.delete(fn);
  }

  get speed() {
    return this._speed;
  }

  setSpeed(newSpeed: number) {
    this._baseReplayMs = this.elapsedReplayMs;
    this._baseWallMs = Date.now();
    this._speed = newSpeed;
  }

  get elapsedReplayMs(): number {
    return this._baseReplayMs + (Date.now() - this._baseWallMs) * this._speed;
  }

  waitUntil(traceTimeMs: number): Promise<void> {
    const remaining = traceTimeMs - this.elapsedReplayMs;
    if (remaining <= 0) return Promise.resolve();
    return new Promise((resolve) =>
      setTimeout(resolve, remaining / this._speed),
    );
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

  constructor(entries: TraceRecord[], player: TracePlayer) {
    (async () => {
      await Promise.resolve(); // yield so caller can set onopen/onmessage
      if (this._closed) return;
      this.onopen?.();

      for (const entry of entries) {
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

    const entries = this._player.findEntries(url);
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
    const entries = this._player.findEntries(url);
    if (!entries || entries.length === 0) {
      // No recorded stream — stay silent rather than opening a real connection.
      return new NoopEventSource();
    }
    return new TraceEventSource(entries, this._player);
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
          for (const entry of entries) {
            if (signal?.aborted) return;
            await player.waitUntil(entry.time);
            if (signal?.aborted) return;
            const text = entry.payload.endsWith("\n\n")
              ? entry.payload
              : entry.payload + "\n\n";
            try {
              controller.enqueue(encoder.encode(text));
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
  loadTraceFromData: (data: TraceData) => void;
  clearTrace: () => void;
  gatewayUrl: string;
  setGatewayUrl: (url: string) => void;
}

export const TransportContext = createContext<TransportContextValue>({
  transport: liveTransport,
  player: null,
  playbackSpeed: 1,
  setPlaybackSpeed: () => {},
  loadTraceFromData: () => {},
  clearTrace: () => {},
  gatewayUrl: DEFAULT_GATEWAY_URL,
  setGatewayUrl: () => {},
});

export function useTransport(): TransportContextValue {
  return useContext(TransportContext);
}

export interface TransportProviderProps {
  children: ReactNode;
  tracePath?: string;
  traceData?: TraceData;
  speed?: number;
}

export function TransportProvider({
  children,
  tracePath,
  traceData: initialTraceData,
  speed = 1,
}: TransportProviderProps) {
  const { enableNetworkRequests } = useUserPreferences();
  const [player, setPlayer] = useState<TracePlayer | null>(null);
  const [playbackSpeed, setPlaybackSpeedState] = useState(speed);
  const [gatewayUrl, setGatewayUrlState] = useState(() => {
    // A user override (set via the UI) wins; otherwise use the value the server
    // injected from its GATEWAY_URL, falling back to the built-in default.
    try {
      const stored = localStorage.getItem("inspector:gatewayUrl");
      if (stored) return stored;
    } catch {
      /* localStorage unavailable */
    }
    const injected = (window as { __HIVE_GATEWAY_URL__?: string })
      .__HIVE_GATEWAY_URL__;
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

  // Bumped (throttled) whenever the player gains records while a trace streams
  // in, so the transport identity changes and consumers re-query for new data.
  const [traceVersion, setTraceVersion] = useState(0);
  useEffect(() => {
    if (!player) return;
    let scheduled = false;
    const unsub = player.subscribe(() => {
      if (scheduled) return;
      scheduled = true;
      setTimeout(() => {
        scheduled = false;
        setTraceVersion((v) => v + 1);
      }, 250);
    });
    return unsub;
  }, [player]);

  const baseTransport = useMemo(
    () =>
      player
        ? new TraceTransport(player)
        : enableNetworkRequests
          ? liveTransport
          : noopTransport,
    // traceVersion intentionally included: a new transport identity makes
    // consumers re-fetch as the streaming trace fills in.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [player, enableNetworkRequests, traceVersion],
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
    if (initialTraceData) setPlayer(new TracePlayer(initialTraceData, speed));
  }, [initialTraceData]);

  useEffect(() => {
    if (!tracePath) return;
    const player = new TracePlayer({}, speed);
    setPlayer(player);
    const ac = new AbortController();

    const addLine = (line: string) => {
      const t = line.trim();
      if (!t) return;
      const { endpoint, ...record } = JSON.parse(t) as {
        endpoint: string;
      } & TraceRecord;
      player.addRecord(endpoint, record);
    };

    (async () => {
      const res = await globalThis.fetch(tracePath, { signal: ac.signal });
      if (!res.body) return;
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const lines = buf.split("\n");
        buf = lines.pop() ?? ""; // keep the trailing partial line
        for (const line of lines) addLine(line);
      }
      addLine(buf); // flush the final line (no trailing newline)
    })().catch((e) => {
      if (!ac.signal.aborted) console.error("Failed to load trace:", e);
    });

    return () => ac.abort();
  }, [tracePath]);

  return (
    <TransportContext.Provider
      value={{
        transport,
        player,
        playbackSpeed,
        setPlaybackSpeed,
        loadTraceFromData,
        clearTrace,
        gatewayUrl,
        setGatewayUrl,
      }}
    >
      {children}
    </TransportContext.Provider>
  );
}
