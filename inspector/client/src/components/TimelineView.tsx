import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ChevronUp } from "lucide-react";
import type { SandboxEvent } from "@/types";
import { humanDuration } from "@/lib/utils";
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


interface MergedGroup {
  bars: TimelineBar[];
  leftPx: number;
  widthPx: number;
}

function computeMergedGroups(
  bars: TimelineBar[],
  toDisplay: (t: number) => number,
  effectiveDurFn: (b: TimelineBar) => number,
): MergedGroup[] {
  if (bars.length === 0) return [];
  const sorted = [...bars].sort((a, b) => a.startTime - b.startTime);
  const groups: MergedGroup[] = [];
  let gBars = [sorted[0]];
  let leftPx = toDisplay(sorted[0].startTime);
  let rightPx = Math.max(leftPx + 1, toDisplay(sorted[0].startTime + effectiveDurFn(sorted[0])));
  for (let i = 1; i < sorted.length; i++) {
    const bar = sorted[i];
    const bLeft = toDisplay(bar.startTime);
    const bRight = Math.max(bLeft + 1, toDisplay(bar.startTime + effectiveDurFn(bar)));
    if (bLeft < rightPx + 1) {
      gBars.push(bar);
      rightPx = Math.max(rightPx, bRight);
    } else {
      groups.push({ bars: gBars, leftPx, widthPx: rightPx - leftPx });
      gBars = [bar]; leftPx = bLeft; rightPx = bRight;
    }
  }
  groups.push({ bars: gBars, leftPx, widthPx: rightPx - leftPx });
  return groups;
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




type ConfigUpdater = (cfg: Record<string, unknown>) => Record<string, unknown>;

export function TimelineView({ events, filter, applyConfig }: { events: SandboxEvent[]; filter: FilterState; applyConfig?: (updater: ConfigUpdater) => Promise<void> }) {
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
  const rowsScrollRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const rowRefMap = useRef<Map<string, HTMLDivElement>>(new Map());
  const selectedBarRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (selectedId !== null) next.set("event", String(selectedId));
      else next.delete("event");
      return next;
    }, { replace: true });
  }, [selectedId]); // eslint-disable-line react-hooks/exhaustive-deps


  const scrollSelectedIntoView = useCallback(() => {
    const barEl = selectedBarRef.current;
    const scrollEl = rowsScrollRef.current;
    if (!barEl || !scrollEl) return;

    const barRect = barEl.getBoundingClientRect();
    const scrollRect = scrollEl.getBoundingClientRect();
    const curTop = scrollEl.scrollTop;
    const curLeft = scrollEl.scrollLeft;

    const hiddenV = barRect.top < scrollRect.top || barRect.bottom > scrollRect.bottom;
    const hiddenH = barRect.left < scrollRect.left + LABEL_W || barRect.right > scrollRect.right;

    if (!hiddenV && !hiddenH) return;

    const barCenterV = (barRect.top  - scrollRect.top)  + curTop  + barRect.height / 2;
    const barCenterH = (barRect.left - scrollRect.left) + curLeft + barRect.width  / 2;

    if (hiddenV) scrollEl.scrollTop = Math.max(0, barCenterV - scrollRect.height / 2);
    if (hiddenH) scrollEl.scrollLeft = Math.max(0, barCenterH - LABEL_W - (scrollRect.width - LABEL_W) / 2);
  }, []);

  useEffect(() => {
    if (selectedId === null) return;
    scrollSelectedIntoView();
  }, [selectedId, scrollSelectedIntoView]);

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

  // Scale: shortest event duration = 1px. Everything else proportional.
  const pxPerMs = (() => {
    let minDur = Infinity;
    for (const row of filteredRows) {
      if (row.isPoint) continue;
      for (const bar of row.bars) {
        const d = effectiveDur(bar);
        if (d > 0) minDur = Math.min(minDur, d);
      }
    }
    if (!isFinite(minDur) || totalSpan <= 0) return trackWidth > 0 ? trackWidth / Math.max(totalSpan, 1) : 1;
    return 1 / minDur;
  })();

  // Gap compression: build segments mapping real time → display pixels.
  // Large gaps are collapsed to GAP_DISPLAY_PX wide with an indicator.
  const GAP_DISPLAY_PX = 20;
  const gapThresholdPx = Math.max(60, 0.15 * trackWidth);

  const rawIntervals: [number, number][] = [];
  for (const row of filteredRows) {
    for (const bar of row.bars) {
      rawIntervals.push([bar.startTime, bar.startTime + bar.durationMs]);
    }
  }
  rawIntervals.sort((a, b) => a[0] - b[0]);
  const mergedIntervals: [number, number][] = [];
  for (const [s, e] of rawIntervals) {
    if (mergedIntervals.length === 0 || s > mergedIntervals[mergedIntervals.length - 1][1]) {
      mergedIntervals.push([s, e]);
    } else {
      mergedIntervals[mergedIntervals.length - 1][1] = Math.max(mergedIntervals[mergedIntervals.length - 1][1], e);
    }
  }

  interface Segment { realStart: number; realEnd: number; dispStart: number; dispEnd: number; isGap: boolean; }
  const segments: Segment[] = [];
  let dispPos = 0;
  let prevEnd = minTime;
  for (const [iStart, iEnd] of mergedIntervals) {
    if (iStart > prevEnd) {
      const gapNatural = (iStart - prevEnd) * pxPerMs;
      const dw = gapNatural > gapThresholdPx ? GAP_DISPLAY_PX : gapNatural;
      segments.push({ realStart: prevEnd, realEnd: iStart, dispStart: dispPos, dispEnd: dispPos + dw, isGap: gapNatural > gapThresholdPx });
      dispPos += dw;
    }
    const evW = Math.max(1, (iEnd - iStart) * pxPerMs);
    segments.push({ realStart: iStart, realEnd: iEnd, dispStart: dispPos, dispEnd: dispPos + evW, isGap: false });
    dispPos += evW;
    prevEnd = Math.max(prevEnd, iEnd);
  }

  // If the natural timeline is shorter than the viewport, scale it up to fill
  // edge-to-edge (with 10px right padding). Otherwise keep the natural scale
  // and let the user scroll.
  const fitScale = trackWidth > 0 && dispPos > 0 && dispPos + 10 < trackWidth
    ? (trackWidth - 10) / dispPos
    : 1;
  for (const seg of segments) {
    seg.dispStart *= fitScale;
    seg.dispEnd   *= fitScale;
  }
const contentTrackWidth = fitScale !== 1 ? trackWidth : Math.ceil(dispPos * fitScale + 10);

  function realToDisplay(realMs: number): number {
    for (const seg of segments) {
      if (realMs <= seg.realEnd) {
        const span = seg.realEnd - seg.realStart;
        return seg.dispStart + (span > 0 ? (realMs - seg.realStart) / span : 0) * (seg.dispEnd - seg.dispStart);
      }
    }
    return contentTrackWidth;
  }

  function displayToReal(dispPx: number): number {
    for (const seg of segments) {
      if (dispPx <= seg.dispEnd) {
        const span = seg.dispEnd - seg.dispStart;
        return seg.realStart + (span > 0 ? (dispPx - seg.dispStart) / span : 0) * (seg.realEnd - seg.realStart);
      }
    }
    return segments.length > 0 ? segments[segments.length - 1].realEnd : minTime;
  }

  // One tick every 100px in display space, skipping positions inside compressed gaps.
  const tickPositions = Array.from(
    { length: Math.floor(contentTrackWidth / 100) + 1 },
    (_, i) => i * 100,
  ).filter(px => !segments.some(s => s.isGap && px > s.dispStart && px < s.dispEnd));

  function syncRuler() {
    if (trackRef.current && rowsScrollRef.current) {
      trackRef.current.scrollLeft = rowsScrollRef.current.scrollLeft;
    }
  }

  return (
    <div className="flex flex-col h-full">

      {/* Sticky ruler — scrolled programmatically to match rows */}
      <div className="shrink-0 text-xs cursor-default select-none border-b border-border">
        <div className="flex" style={{ paddingLeft: LABEL_W }}>
          <div ref={trackRef} className="flex-1 overflow-hidden">
            <div className="group/ruler relative h-6" style={{ width: contentTrackWidth }}>
              {/* Compressed-gap indicators */}
              {segments.filter(s => s.isGap).map((seg, i) => (
                <div
                  key={`gap-${i}`}
                  className="gap-indicator group/gap absolute top-0 bottom-0 flex items-center justify-center border-x border-dashed border-zinc-500/30 bg-zinc-500/10 hover:bg-zinc-500/20 transition-colors"
                  style={{ left: seg.dispStart, width: seg.dispEnd - seg.dispStart }}
                >
                  <span className="text-[9px] text-zinc-400 whitespace-nowrap opacity-0 group-hover/gap:opacity-100 transition-opacity">
                    ~{humanDuration(seg.realEnd - seg.realStart)}
                  </span>
                </div>
              ))}
              {/* Time ticks — one every 100px in display space */}
              {tickPositions.map((px, i) => {
                const isFirst = i === 0;
                const isLast  = i === tickPositions.length - 1;
                return (
                  <div
                    key={px}
                    className={`absolute top-0 flex flex-col transition-opacity group-has-[.gap-indicator:hover]/ruler:opacity-0 ${isLast ? "items-end" : isFirst ? "items-start" : "items-center"}`}
                    style={isFirst ? { left: 0 } : isLast ? { right: 0 } : { left: px, transform: "translateX(-50%)" }}
                  >
                    <span className="whitespace-nowrap text-muted-foreground">{humanDuration(displayToReal(px) - minTime)}</span>
                    <div className="h-1.5 w-px bg-border" />
                  </div>
                );
              })}
            </div>
          </div>
        </div>
      </div>

      {/* Rows — horizontal + vertical scroll */}
      <div
        ref={rowsScrollRef}
        className="timeline-scroll min-h-0 flex-1 overflow-auto text-xs cursor-default select-none"
        onScroll={syncRuler}
      >
        <div className="relative" style={{ width: LABEL_W + contentTrackWidth }}>
          {filteredRows.map((row) => {
            const isRowSelected = row.bars.some(b => b.id === selectedId);

            return (
              <div
                key={row.key}
                ref={(el) => { if (el) rowRefMap.current.set(row.key, el); else rowRefMap.current.delete(row.key); }}
                className={`group flex border-b border-border/40 ${isRowSelected ? "bg-accent/60" : "hover:bg-muted/30"}`}
                style={{ height: 22 }}
              >
                {/* Label column — sticky so it stays visible when scrolling horizontally */}
                <div
                  className={`shrink-0 sticky left-0 z-10 flex items-center gap-1.5 overflow-hidden px-5 ${isRowSelected ? "bg-accent/60" : "bg-background group-hover:bg-muted/30"}`}
                  style={{ width: LABEL_W }}
                >
                  <span className={`shrink-0 font-mono font-semibold ${methodClass(row)}`}>
                    {row.method?.toUpperCase()}
                  </span>
                  <span className="truncate font-mono text-muted-foreground">{row.label}</span>
                </div>

                {/* Track */}
                <div className="relative self-stretch overflow-hidden" style={{ width: contentTrackWidth }}>
                  {/* Grid lines */}
                  {tickPositions.map((px) => (
                    <div key={px} className="absolute inset-y-0 w-px bg-border/30" style={{ left: px }} />
                  ))}

                  {row.isPoint ? (
                    row.bars.map(bar => (
                      <div
                        key={bar.id}
                        ref={bar.id === selectedId ? (el) => { selectedBarRef.current = el; } : undefined}
                        className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 rounded-sm cursor-pointer ${row.method === "err" ? "bg-red-400/70" : "bg-zinc-500/70"} ${selectedId !== null ? "opacity-50" : ""}`}
                        style={{ left: realToDisplay(bar.startTime), width: 1, maxWidth: `calc(100% - ${realToDisplay(bar.startTime)}px)` }}
                        title={row.label}
                        onClick={() => setSelectedId(bar.id === selectedId ? null : bar.id)}
                      />
                    ))
                  ) : (
                    computeMergedGroups(row.bars, realToDisplay, effectiveDur).map((group) => {
                      const { bars: gBars, leftPx, widthPx } = group;
                      const isSingle = gBars.length === 1;
                      const firstBar = gBars[0];
                      const selectedIdx = gBars.findIndex(b => b.id === selectedId);
                      const isGroupSelected = selectedIdx !== -1;
                      const isLive = gBars.some(b => b.pending && b.access === "allowed");
                      const isError = gBars.some(b =>
                        b.access === "denied" || !!b.error || (b.status !== undefined && b.status >= 400));
                      const isPending = !isLive && gBars.some(b => b.pending);

                      const visualClass = isError
                        ? "bg-red-500/80"
                        : isPending
                          ? "bg-muted-foreground/40 border border-dashed border-muted-foreground/60"
                          : isSingle
                            ? barClass(firstBar, row.type)
                            : row.type === "egress" ? "bg-blue-500/80" : "bg-purple-500/80";

                      function handleClick() {
                        if (selectedIdx === -1) setSelectedId(firstBar.id);
                        else if (selectedIdx < gBars.length - 1) setSelectedId(gBars[selectedIdx + 1].id);
                        else setSelectedId(null);
                      }

                      return (
                        <div
                          key={firstBar.id}
                          ref={isGroupSelected ? (el) => { selectedBarRef.current = el; } : undefined}
                          className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 cursor-pointer ${!isGroupSelected && selectedId !== null ? "opacity-50" : ""}`}
                          style={{ left: leftPx, width: widthPx, maxWidth: `calc(100% - ${leftPx}px)` }}
                          onClick={handleClick}
                        >
                          <div
                            className={`h-full w-full rounded-sm transition-none overflow-hidden relative ${isLive ? "bg-blue-500/50 border border-blue-400/60" : visualClass}`}
                            title={isSingle
                              ? (isLive ? `in-flight · ${effectiveDur(firstBar)}ms` : firstBar.pending ? "no response received" : `${firstBar.durationMs}ms${firstBar.status ? ` · ${firstBar.status}` : ""}${firstBar.error ? ` · ${firstBar.error}` : ""}`)
                              : `${gBars.length} events`}
                          >
                            {isLive && <div className="absolute inset-0 bar-in-flight" />}
                          </div>
                        </div>
                      );
                    })
                  )}
                </div>
              </div>
            );
          })}
          <div ref={bottomRef} />
        </div>
      </div>

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
