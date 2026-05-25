import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ChevronUp } from "lucide-react";
import { ScrollArea } from "@/components/ui/scroll-area";
import type { SandboxEvent } from "@/types";
import { RowDetailPanel } from "./TimelineDetail";

export interface TimelineBar {
  id: number;
  startTime: number;
  durationMs: number;
  status?: number;
  access?: "allowed" | "denied";
  error?: string;
  pending: boolean;
  rawEvents: SandboxEvent[];
}

export interface TimelineRow {
  key: string;
  type: "egress" | "fs" | "stdio";
  label: string;
  method?: string;
  isPoint: boolean;
  bars: TimelineBar[];
}

export function buildRows(events: SandboxEvent[]): TimelineRow[] {
  const chunkMap = new Map<number, Extract<SandboxEvent, { type: "egress.chunk" }>[]>();
  const egressResMap = new Map<number, Extract<SandboxEvent, { type: "egress.response" }>>();
  const fsResMap = new Map<number, Extract<SandboxEvent, { type: "fs.response" }>>();
  const rowMap = new Map<string, TimelineRow>();
  const rowOrder: string[] = [];

  for (const event of events) {
    if (event.type === "egress.chunk") {
      const list = chunkMap.get(event.request_id) ?? [];
      list.push(event);
      chunkMap.set(event.request_id, list);
    } else if (event.type === "egress.response") {
      egressResMap.set(event.request_id, event);
    } else if (event.type === "fs.response") {
      fsResMap.set(event.request_id, event);
    }
  }

  function getOrCreateRow(key: string, type: TimelineRow["type"], label: string, method?: string, isPoint = false): TimelineRow {
    if (!rowMap.has(key)) {
      rowMap.set(key, { key, type, label, method, isPoint, bars: [] });
      rowOrder.push(key);
    }
    return rowMap.get(key)!;
  }

  for (const event of events) {
    if (event.type === "egress.request") {
      const res = egressResMap.get(event.id);
      const chunks = chunkMap.get(event.id) ?? [];
      const startMs = new Date(event.timestamp).getTime();
      const lastChunk = chunks[chunks.length - 1];
      const durationMs = res
        ? (lastChunk ? new Date(lastChunk.timestamp).getTime() - startMs : res.duration_ms)
        : 0;
      const label = `${event.host}${event.path}`;
      const key = `egress:${event.method}:${label}`;
      const row = getOrCreateRow(key, "egress", label, event.method);
      row.bars.push({
        id: event.id,
        startTime: startMs,
        durationMs,
        status: res?.status,
        access: event.access,
        pending: !res,
        rawEvents: res ? [event, res, ...chunks] : [event, ...chunks],
      });
    } else if (event.type === "fs.request") {
      const res = fsResMap.get(event.id);
      const key = `fs:${event.mount}:${event.operation}`;
      const row = getOrCreateRow(key, "fs", event.mount, event.operation);
      row.bars.push({
        id: event.id,
        startTime: new Date(event.timestamp).getTime(),
        durationMs: res?.duration_ms ?? 0,
        access: event.access,
        error: res?.error,
        pending: !res,
        rawEvents: res ? [event, res] : [event],
      });
    } else if (event.type === "stdio") {
      const text = (event.stdout ?? event.stderr ?? "").trimEnd();
      const method = event.stderr ? "err" : "out";
      // Each stdio event stays on its own row (unique content per event).
      const key = `stdio:${event.id}`;
      const row = getOrCreateRow(key, "stdio", text.slice(0, 120), method, true);
      row.bars.push({
        id: event.id,
        startTime: new Date(event.timestamp).getTime(),
        durationMs: 0,
        pending: false,
        rawEvents: [event],
      });
    }
  }

  // For fs rows, all rows sharing a mount sort together by the mount's earliest event.
  const mountEarliestTime = new Map<string, number>();
  for (const k of rowOrder) {
    const row = rowMap.get(k)!;
    if (row.type === "fs") {
      const t = row.bars[0]?.startTime ?? Infinity;
      const prev = mountEarliestTime.get(row.label) ?? Infinity;
      if (t < prev) mountEarliestTime.set(row.label, t);
    }
  }

  return rowOrder
    .map(k => rowMap.get(k)!)
    .sort((a, b) => {
      const aTime = a.type === "fs" ? (mountEarliestTime.get(a.label) ?? 0) : (a.bars[0]?.startTime ?? 0);
      const bTime = b.type === "fs" ? (mountEarliestTime.get(b.label) ?? 0) : (b.bars[0]?.startTime ?? 0);
      if (aTime !== bTime) return aTime - bTime;
      // Within the same mount, sort read before write.
      if (a.type === "fs" && b.type === "fs" && a.label === b.label)
        return (a.method ?? "").localeCompare(b.method ?? "");
      return 0;
    });
}

function computeBarPositions(
  bars: TimelineBar[],
  minTime: number,
  totalSpan: number,
  trackWidth: number,
  effectiveDurFn: (bar: TimelineBar) => number,
): { leftPx: number; widthPx: number }[] {
  if (trackWidth <= 0) return bars.map(() => ({ leftPx: 0, widthPx: 1 }));
  const pxPerMs = trackWidth / totalSpan;
  const result: { leftPx: number; widthPx: number }[] = new Array(bars.length);
  const order = bars.map((_, i) => i).sort((a, b) => bars[a].startTime - bars[b].startTime);
  let rightEdgePx = -Infinity;
  for (const i of order) {
    const bar = bars[i];
    const widthPx = Math.max(1, effectiveDurFn(bar) * pxPerMs);
    const leftPx = Math.max((bar.startTime - minTime) * pxPerMs, rightEdgePx + 1);
    rightEdgePx = leftPx + widthPx;
    result[i] = { leftPx, widthPx };
  }
  return result;
}

function barClass(bar: TimelineBar, type: "egress" | "fs" | "stdio"): string {
  if (bar.pending) return "bg-muted-foreground/40 border border-dashed border-muted-foreground/60";
  if (bar.access === "denied" || bar.error) return "bg-red-500/80";
  if (type === "egress") {
    return bar.status !== undefined && bar.status >= 400 ? "bg-red-500/80" : "bg-blue-500/80";
  }
  return "bg-purple-500/80";
}

function methodClass(row: TimelineRow): string {
  if (row.type === "stdio") return row.method === "err" ? "text-red-400" : "text-zinc-400";
  if (row.type === "fs") return "text-purple-400";
  switch (row.method) {
    case "GET":    return "text-green-400";
    case "POST":   return "text-blue-400";
    case "PUT":
    case "PATCH":  return "text-orange-400";
    case "DELETE": return "text-red-400";
    default:       return "text-muted-foreground";
  }
}


export type FilterKind = "all" | "egress" | "fs" | "llm";
export type FilterAccess = "all" | "allowed" | "denied";

export const KIND_OPTIONS: { value: FilterKind; label: string }[] = [
  { value: "all",    label: "All" },
  { value: "egress", label: "Egress" },
  { value: "fs",     label: "File system" },
  { value: "llm",    label: "LLM" },
];

export const ACCESS_OPTIONS: { value: FilterAccess; label: string }[] = [
  { value: "all",     label: "Any" },
  { value: "allowed", label: "Allowed" },
  { value: "denied",  label: "Denied" },
];

export interface FilterState { kind: FilterKind; access: FilterAccess; query: string }
export const EMPTY_FILTER: FilterState = { kind: "all", access: "all", query: "" };

export function isFilterActive(f: FilterState) { return f.kind !== "all" || f.access !== "all" || f.query !== ""; }

export function applyFilter(rows: TimelineRow[], f: FilterState): TimelineRow[] {
  let out = rows;
  if (f.kind === "fs") out = out.filter((r) => r.type === "fs");
  else if (f.kind === "llm") out = out.filter((r) => {
    const e = r.bars[0]?.rawEvents[0];
    return e?.type === "egress.request" && e.host === "api.anthropic.com" && e.path === "/v1/messages";
  });
  else if (f.kind === "egress") out = out.filter((r) => r.type === "egress");
  if (f.query) {
    const q = f.query.toLowerCase();
    out = out.filter((r) => r.label.toLowerCase().includes(q));
  }
  if (f.access === "allowed" || f.access === "denied") {
    out = out
      .map(r => ({ ...r, bars: r.bars.filter(b => b.access === f.access) }))
      .filter(r => r.bars.length > 0);
  }
  return out;
}

export function filterEvents(events: SandboxEvent[], f: FilterState): SandboxEvent[] {
  let out = events;

  if (f.kind !== "all") {
    const llmIds = f.kind === "llm"
      ? new Set(events
          .filter(e => e.type === "egress.request" && e.host === "api.anthropic.com" && e.path === "/v1/messages")
          .map(e => e.id))
      : null;
    out = out.filter(e => {
      if (f.kind === "egress") return e.type === "egress.request" || e.type === "egress.response" || e.type === "egress.chunk";
      if (f.kind === "fs") return e.type === "fs.request" || e.type === "fs.response";
      if (f.kind === "llm") {
        if (e.type === "egress.request") return llmIds!.has(e.id);
        if (e.type === "egress.response" || e.type === "egress.chunk") return llmIds!.has(e.request_id);
        return false;
      }
      return true;
    });
  }

  if (f.access === "denied") out = out.filter(e => e.type === "egress.request" || e.type === "fs.request" ? e.access === "denied" : false);
  else if (f.access === "allowed") out = out.filter(e => e.type === "egress.request" || e.type === "fs.request" ? e.access === "allowed" : true);

  if (f.query) {
    const q = f.query.toLowerCase();
    out = out.filter(e => {
      if (e.type === "egress.request") return `${e.host}${e.path}`.toLowerCase().includes(q);
      if (e.type === "fs.request") return e.path.toLowerCase().includes(q);
      if (e.type === "stdio") return (e.stdout ?? e.stderr ?? "").toLowerCase().includes(q);
      return true;
    });
  }

  return out;
}

const LABEL_W = 220;
const BAR_MIN_W = "1px";

interface DurationLabelProps {
  dur: number;
  leftNum: number;
  rightEdgeNum: number;
  rowIdx: number;
  totalRows: number;
  isLive: boolean;
  isError: boolean;
  isPending: boolean;
  status?: number;
}

function DurationLabel({ dur, leftNum, rightEdgeNum, rowIdx, totalRows, isLive, isError, isPending, status }: DurationLabelProps) {
  let posClass: string;
  if (rightEdgeNum < 0.68) {
    posClass = "left-full top-1/2 -translate-y-1/2 pl-1.5";
  } else if (leftNum > 0.06) {
    posClass = "right-full top-1/2 -translate-y-1/2 pr-1.5";
  } else if (rowIdx < Math.floor(totalRows / 2)) {
    posClass = "top-full left-0 mt-0.5";
  } else {
    posClass = "bottom-full left-0 mb-0.5";
  }

  const colorClass = isError ? "text-red-400" : isLive ? "text-blue-400" : "text-muted-foreground";

  const text = isLive
    ? dur < 1000 ? `${dur}ms…` : `${(dur / 1000).toFixed(2)}s…`
    : isPending
      ? "no response"
      : dur < 1000 ? `${dur}ms` : `${(dur / 1000).toFixed(2)}s`;

  return (
    <span className={`absolute z-10 font-mono whitespace-nowrap opacity-0 group-hover/bar:opacity-100 transition-opacity ${posClass} ${colorClass}`}>
      {text}{status ? ` ${status}` : ""}
    </span>
  );
}


function fmtTick(ms: number): string {
  if (ms === 0) return "0";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)}s`;
}

type ConfigUpdater = (cfg: Record<string, unknown>) => Record<string, unknown>;

export function TimelineView({ events, filter, autoScroll, applyConfig }: { events: SandboxEvent[]; filter: FilterState; autoScroll?: boolean; applyConfig?: (updater: ConfigUpdater) => Promise<void> }) {
  const rows = useMemo(() => buildRows(events), [events]);

  const hasLive = rows.some((r) => r.bars.some(b => b.pending && b.access === "allowed"));
  const [, forceUpdate] = useState(0);
  const [trackWidth, setTrackWidth] = useState(600);
  const [searchParams, setSearchParams] = useSearchParams();

  const [selectedId, setSelectedId] = useState<number | null>(() => {
    const v = searchParams.get("event");
    return v ? parseInt(v, 10) : null;
  });
  const [panelCollapsed, setPanelCollapsed] = useState(
    () => localStorage.getItem("timeline:panelCollapsed") === "true",
  );

  useEffect(() => {
    localStorage.setItem("timeline:panelCollapsed", String(panelCollapsed));
  }, [panelCollapsed]);
  const trackRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const rowRefMap = useRef<Map<string, HTMLDivElement>>(new Map());

  useEffect(() => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (selectedId !== null) next.set("event", String(selectedId));
      else next.delete("event");
      return next;
    }, { replace: true });
  }, [selectedId]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (autoScroll) {
      bottomRef.current?.scrollIntoView({ behavior: "smooth" });
    }
  }, [rows.length, autoScroll]);

  useEffect(() => {
    if (selectedId === null) return;
    const row = rows.find(r => r.bars.some(b => b.id === selectedId));
    if (row) rowRefMap.current.get(row.key)?.scrollIntoView({ block: "nearest" });
  }, [selectedId, rows]);

  // Wall-clock time when the most recent SSE event arrived at the client.
  const lastEventReceivedRef = useRef(Date.now());

  // Observe the ruler track div to derive responsive tick count.
  useEffect(() => {
    const el = trackRef.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => setTrackWidth(entry.contentRect.width));
    ro.observe(el);
    return () => ro.disconnect();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows.length > 0]);

  // Reset the elapsed clock each time a new event arrives.
  useEffect(() => {
    lastEventReceivedRef.current = Date.now();
  }, [events.length]);

  // Tick while any in-flight request is pending.
  useEffect(() => {
    if (!hasLive) return;
    const id = setInterval(() => forceUpdate((n) => n + 1), 100);
    return () => clearInterval(id);
  }, [hasLive]);

  const selectedBar = selectedId !== null
    ? rows.flatMap(r => r.bars).find(b => b.id === selectedId) ?? null
    : null;

  // Flat list of all Anthropic bars for "previous context" comparison in the detail panel.
  const anthropicBars = useMemo(() =>
    rows
      .filter(r => {
        const e = r.bars[0]?.rawEvents[0];
        return e?.type === "egress.request" && e.host === "api.anthropic.com" && e.path === "/v1/messages";
      })
      .flatMap(r => r.bars),
  [rows]);

  const prevAnthropicBar = (() => {
    const idx = anthropicBars.findIndex(b => b.id === selectedId);
    return idx > 0 ? anthropicBars[idx - 1] : null;
  })();

  if (rows.length === 0) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        No events yet.
      </div>
    );
  }

  const filteredRows = applyFilter(rows, filter);

  if (filteredRows.length === 0) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        No events match the current filter.
      </div>
    );
  }

  // Flat ordered list of bars for prev/next navigation.
  const filteredBars = filteredRows.flatMap(r => r.bars);
  const selectedBarIdx = filteredBars.findIndex(b => b.id === selectedId);
  const prevBarId = selectedBarIdx > 0 ? filteredBars[selectedBarIdx - 1].id : null;
  const nextBarId = selectedBarIdx >= 0 && selectedBarIdx < filteredBars.length - 1
    ? filteredBars[selectedBarIdx + 1].id : null;

  const now = Date.now();
  const elapsed = hasLive ? Math.max(0, now - lastEventReceivedRef.current) : 0;

  const minTime = Math.min(...filteredBars.map(b => b.startTime));
  const maxEventEnd = Math.max(...filteredBars.map(b => b.startTime + b.durationMs), minTime + 1);
  const rightEdge = maxEventEnd + elapsed;
  const rawSpan = Math.max(rightEdge - minTime, 1);
  // Right-side padding: at least 30px worth of time so the last bar is never clipped.
  const rightPad = trackWidth > 0 ? (30 / trackWidth) * rawSpan : rawSpan * 0.03;
  const totalSpan = rawSpan + rightPad;

  function effectiveDur(bar: TimelineBar): number {
    if (bar.pending && bar.access === "allowed") {
      return Math.max(totalSpan - (bar.startTime - minTime), 0);
    }
    return bar.durationMs;
  }

  const tickCount = Math.min(12, Math.max(3, Math.floor(trackWidth / 90)));
  const tickInterval = totalSpan / tickCount;
  const ticks = Array.from({ length: tickCount + 1 }, (_, i) => i * tickInterval);

  function pct(ms: number) {
    return `${(ms / totalSpan) * 100}%`;
  }

  return (
    <div className="flex flex-col h-full">

      {/* Sticky ruler */}
      <div className="shrink-0 text-xs cursor-default select-none border-b border-border">
        <div className="flex" style={{ paddingLeft: LABEL_W }}>
          <div ref={trackRef} className="relative h-6 flex-1">
            {ticks.map((t, i) => {
              const isFirst = i === 0;
              const isLast  = i === ticks.length - 1;
              return (
                <div
                  key={t}
                  className={`absolute top-0 flex flex-col ${isLast ? "items-end right-0" : isFirst ? "items-start left-0" : "items-center"}`}
                  style={isFirst || isLast ? undefined : { left: pct(t), transform: "translateX(-50%)" }}
                >
                  <span className="whitespace-nowrap text-muted-foreground">
                    {fmtTick(t)}
                  </span>
                  <div className="h-1.5 w-px bg-border" />
                </div>
              );
            })}
          </div>
        </div>
      </div>

    <ScrollArea className="min-h-0 flex-1">
      <div className="text-xs cursor-default select-none">

        {/* Rows */}
        <div className="border-border relative">
          {filteredRows.map((row, rowIdx) => {
            const isRowSelected = row.bars.some(b => b.id === selectedId);
            const barPositions = row.isPoint ? null : computeBarPositions(row.bars, minTime, totalSpan, trackWidth, effectiveDur);

            return (
              <div
                key={row.key}
                ref={(el) => { if (el) rowRefMap.current.set(row.key, el); else rowRefMap.current.delete(row.key); }}
                className={`flex items-center border-b border-border/40 ${isRowSelected ? "bg-accent/60" : "hover:bg-muted/30"}`}
                style={{ height: 22 }}
              >
                {/* Label column */}
                <div
                  className="flex shrink-0 items-center gap-1.5 overflow-hidden px-5"
                  style={{ width: LABEL_W }}
                >
                  <span className={`shrink-0 font-mono font-semibold ${methodClass(row)}`}>
                    {row.method?.toUpperCase()}
                  </span>
                  <span className="truncate font-mono text-muted-foreground">{row.label}</span>
                </div>

                {/* Track */}
                <div className="relative flex-1 self-stretch overflow-hidden">
                  {/* Grid lines */}
                  {ticks.map((t) => (
                    <div
                      key={t}
                      className="absolute inset-y-0 w-px bg-border/30"
                      style={{ left: pct(t) }}
                    />
                  ))}

                  {row.isPoint ? (
                    row.bars.map(bar => (
                      <div
                        key={bar.id}
                        className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 rounded-sm cursor-pointer ${row.method === "err" ? "bg-red-400/70" : "bg-zinc-500/70"} ${bar.id === selectedId ? "ring-1 ring-white/50" : selectedId !== null ? "opacity-50" : ""}`}
                        style={{ left: pct(bar.startTime - minTime), width: `max(${BAR_MIN_W},0%)`, maxWidth: `calc(100% - ${pct(bar.startTime - minTime)})` }}
                        title={row.label}
                        onClick={() => setSelectedId(bar.id === selectedId ? null : bar.id)}
                      />
                    ))
                  ) : (
                    row.bars.map((bar, barIdx) => {
                      const { leftPx, widthPx } = barPositions![barIdx];
                      const dur = effectiveDur(bar);
                      const isLive = bar.pending && bar.access === "allowed";
                      const isBarSelected = bar.id === selectedId;
                      const leftNum = trackWidth > 0 ? leftPx / trackWidth : 0;
                      const rightEdgeNum = trackWidth > 0 ? Math.min((leftPx + widthPx) / trackWidth, 1) : 0;
                      const isError =
                        bar.access === "denied" ||
                        !!bar.error ||
                        (bar.status !== undefined && bar.status >= 400);

                      return (
                        <div
                          key={bar.id}
                          className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 cursor-pointer ${!isBarSelected && selectedId !== null ? "opacity-50" : ""}`}
                          style={{ left: leftPx, width: widthPx, maxWidth: `calc(100% - ${leftPx}px)` }}
                          onClick={() => setSelectedId(isBarSelected ? null : bar.id)}
                        >
                          {/* Bar visual — overflow-hidden clips the stripe animation */}
                          <div
                            className={`h-full w-full rounded-sm transition-none overflow-hidden relative ${isLive ? "bg-blue-500/50 border border-blue-400/60" : barClass(bar, row.type)} ${isBarSelected ? "ring-1 ring-white/50" : ""}`}
                            title={
                              isLive
                                ? `in-flight · ${dur}ms`
                                : bar.pending
                                  ? "no response received"
                                  : `${bar.durationMs}ms${bar.status ? ` · ${bar.status}` : ""}${bar.error ? ` · ${bar.error}` : ""}`
                            }
                          >
                            {isLive && <div className="absolute inset-0 bar-in-flight" />}
                          </div>
                          <DurationLabel
                            dur={dur}
                            leftNum={leftNum}
                            rightEdgeNum={rightEdgeNum}
                            rowIdx={rowIdx}
                            totalRows={filteredRows.length}
                            isLive={isLive}
                            isError={isError}
                            isPending={bar.pending}
                            status={bar.status}
                          />
                        </div>
                      );
                    })
                  )}
                </div>
              </div>
            );
          })}
        </div>
      </div>
      <div ref={bottomRef} />
    </ScrollArea>

    {selectedBar && (
      panelCollapsed ? (
        <div className="shrink-0 border-t border-border flex items-center px-3 h-7">
          <button onClick={() => setPanelCollapsed(false)} className="text-muted-foreground/50 hover:text-muted-foreground transition-colors">
            <ChevronUp className="h-3.5 w-3.5" />
          </button>
        </div>
      ) : (
        <div className="h-[45%] shrink-0 border-t border-border flex flex-col overflow-hidden">
          <RowDetailPanel
            key={selectedBar.id}
            bar={selectedBar}
            prevBar={prevAnthropicBar}
            onPrev={prevBarId !== null ? () => setSelectedId(prevBarId) : undefined}
            onNext={nextBarId !== null ? () => setSelectedId(nextBarId) : undefined}
            applyConfig={applyConfig}
          />
        </div>
      )
    )}
    </div>
  );
}
