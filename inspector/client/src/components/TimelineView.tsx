import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ChevronUp } from "lucide-react";
import { ScrollArea } from "@/components/ui/scroll-area";
import type { SandboxEvent } from "@/types";
import { RowDetailPanel } from "./TimelineDetail";

export interface TimelineRow {
  id: number;
  type: "egress" | "fs" | "stdio";
  label: string;
  method?: string;
  startTime: number; // epoch ms
  durationMs: number; // 0 for point events
  status?: number;
  access?: "allowed" | "denied";
  error?: string;
  pending: boolean; // request with no response
  isPoint: boolean; // no duration — render as a tick marker
  rawEvents: SandboxEvent[]; // source events for the detail panel
}

export function buildRows(events: SandboxEvent[]): TimelineRow[] {
  const requestMap = new Map<number, SandboxEvent>();
  const responded = new Set<number>();
  const chunkMap = new Map<number, SandboxEvent[]>();
  const rows: TimelineRow[] = [];

  for (const event of events) {
    if (event.type === "egress.stream_chunk") {
      const list = chunkMap.get(event.request_id) ?? [];
      list.push(event);
      chunkMap.set(event.request_id, list);
      continue;
    }
    if (event.type === "egress.request" || event.type === "fs.request") {
      requestMap.set(event.id, event);
    } else if (event.type === "egress.response") {
      const req = requestMap.get(event.request_id);
      if (req?.type === "egress.request") {
        responded.add(event.request_id);
        rows.push({
          id: req.id,
          type: "egress",
          label: `${req.host}${req.path}`,
          method: req.method,
          startTime: new Date(req.timestamp).getTime(),
          durationMs: event.duration_ms,
          status: event.status,
          access: req.access,
          pending: false,
          isPoint: false,
          rawEvents: [req, event, ...(chunkMap.get(req.id) ?? [])],
        });
      }
    } else if (event.type === "fs.response") {
      const req = requestMap.get(event.request_id);
      if (req?.type === "fs.request") {
        responded.add(event.request_id);
        rows.push({
          id: req.id,
          type: "fs",
          label: req.path,
          method: req.operation,
          startTime: new Date(req.timestamp).getTime(),
          durationMs: event.duration_ms,
          access: req.access,
          error: event.error,
          pending: false,
          isPoint: false,
          rawEvents: [req, event],
        });
      }
    } else if (event.type === "stdio") {
      const text = (event.stdout ?? event.stderr ?? "").trimEnd();
      rows.push({
        id: event.id,
        type: "stdio",
        label: text.slice(0, 120),
        method: event.stderr ? "err" : "out",
        startTime: new Date(event.timestamp).getTime(),
        durationMs: 0,
        pending: false,
        isPoint: true,
        rawEvents: [event],
      });
    }
  }

  // Requests with no response (failed or still in-flight)
  for (const [id, event] of requestMap) {
    if (responded.has(id)) continue;
    if (event.type === "egress.request") {
      rows.push({
        id: event.id,
        type: "egress",
        label: `${event.host}${event.path}`,
        method: event.method,
        startTime: new Date(event.timestamp).getTime(),
        durationMs: 0,
        access: event.access,
        pending: true,
        isPoint: false,
        rawEvents: [event, ...(chunkMap.get(event.id) ?? [])],
      });
    } else if (event.type === "fs.request") {
      rows.push({
        id: event.id,
        type: "fs",
        label: event.path,
        method: event.operation,
        startTime: new Date(event.timestamp).getTime(),
        durationMs: 0,
        access: event.access,
        pending: true,
        isPoint: false,
        rawEvents: [event],
      });
    }
  }

  return rows.sort((a, b) => a.startTime - b.startTime);
}

function barClass(row: TimelineRow): string {
  if (row.isPoint) return ""; // handled separately
  if (row.pending) return "bg-muted-foreground/40 border border-dashed border-muted-foreground/60";
  if (row.access === "denied" || row.error) return "bg-red-500/80";
  if (row.type === "egress") {
    return row.status !== undefined && row.status >= 400 ? "bg-red-500/80" : "bg-blue-500/80";
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
    const e = r.rawEvents[0];
    return e.type === "egress.request" && e.host === "api.anthropic.com" && e.path === "/v1/messages";
  });
  else if (f.kind === "egress") out = out.filter((r) => r.type === "egress");
  if (f.access === "allowed") out = out.filter((r) => r.access === "allowed");
  else if (f.access === "denied") out = out.filter((r) => r.access === "denied");
  if (f.query) {
    const q = f.query.toLowerCase();
    out = out.filter((r) => r.label.toLowerCase().includes(q));
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
      if (f.kind === "egress") return e.type === "egress.request" || e.type === "egress.response" || e.type === "egress.stream_chunk";
      if (f.kind === "fs") return e.type === "fs.request" || e.type === "fs.response";
      if (f.kind === "llm") {
        if (e.type === "egress.request") return llmIds!.has(e.id);
        if (e.type === "egress.response" || e.type === "egress.stream_chunk") return llmIds!.has(e.request_id);
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

interface DurationLabelProps {
  dur: number;
  leftNum: number;    // bar left edge as 0..1 fraction of totalSpan
  rightEdgeNum: number; // bar right edge as 0..1
  rowIdx: number;
  totalRows: number;
  isLive: boolean;
  isError: boolean;
  isPending: boolean;
  status?: number;
}

function DurationLabel({ dur, leftNum, rightEdgeNum, rowIdx, totalRows, isLive, isError, isPending, status }: DurationLabelProps) {
  // Pick the direction with the most room, in priority order:
  //   right → left → below → above
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
    <span className={`absolute z-10 font-mono whitespace-nowrap opacity-0 group-hover:opacity-100 transition-opacity ${posClass} ${colorClass}`}>
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

  const hasLive = rows.some((r) => r.pending && r.access === "allowed");
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
  const rowRefMap = useRef<Map<number, HTMLDivElement>>(new Map());

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
    rowRefMap.current.get(selectedId)?.scrollIntoView({ block: "nearest" });
  }, [selectedId]);

  // Wall-clock time when the most recent SSE event arrived at the client.
  const lastEventReceivedRef = useRef(Date.now());

  // Observe the ruler track div to derive responsive tick count.
  // Depend on rows.length > 0 so the effect re-runs once the ref'd div actually mounts
  // (the early "No events" return renders a different element, leaving trackRef null).
  useEffect(() => {
    const el = trackRef.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => setTrackWidth(entry.contentRect.width));
    ro.observe(el);
    return () => ro.disconnect();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows.length > 0 /* re-mount observer once rows appear */]);

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

  const selectedRow = selectedId !== null ? rows.find((r) => r.id === selectedId) ?? null : null;

  const anthropicRows = useMemo(() =>
    rows.filter((r) => {
      const e = r.rawEvents[0];
      return e.type === "egress.request" && e.host === "api.anthropic.com" && e.path === "/v1/messages";
    }),
  [rows]);

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

  const selectedFilteredIdx = filteredRows.findIndex((r) => r.id === selectedId);
  const prevFilteredRow = selectedFilteredIdx > 0 ? filteredRows[selectedFilteredIdx - 1] : null;
  const nextFilteredRow = selectedFilteredIdx >= 0 && selectedFilteredIdx < filteredRows.length - 1
    ? filteredRows[selectedFilteredIdx + 1] : null;

  const prevAnthropicRow = (() => {
    const idx = anthropicRows.findIndex((r) => r.id === selectedId);
    return idx > 0 ? anthropicRows[idx - 1] : null;
  })();

  const now = Date.now();
  const elapsed = hasLive ? Math.max(0, now - lastEventReceivedRef.current) : 0;

  const minTime = Math.min(...filteredRows.map((r) => r.startTime));
  const maxEventEnd = Math.max(...filteredRows.map((r) => r.startTime + r.durationMs), minTime + 1);
  const rightEdge = maxEventEnd + elapsed;
  const rawSpan = Math.max(rightEdge - minTime, 1);
  // Right-side padding: at least 30px worth of time so the last bar is never clipped.
  const rightPad = trackWidth > 0 ? (30 / trackWidth) * rawSpan : rawSpan * 0.03;
  const totalSpan = rawSpan + rightPad;

  function effectiveDur(row: TimelineRow): number {
    if (row.pending && row.access === "allowed") {
      return Math.max(totalSpan - (row.startTime - minTime), 0);
    }
    return row.durationMs;
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
            const dur = effectiveDur(row);
            const isLive = row.pending && row.access === "allowed";
            const leftPct = pct(row.startTime - minTime);
            const widthPct = pct(dur);
            const leftNum = (row.startTime - minTime) / totalSpan;
            const rightEdgeNum = Math.min(leftNum + Math.max(dur, 0) / totalSpan, 1);
            const isError =
              row.access === "denied" ||
              !!row.error ||
              (row.status !== undefined && row.status >= 400);

            const isSelected = selectedId === row.id;

            return (
              <div
                key={row.id}
                ref={(el) => { if (el) rowRefMap.current.set(row.id, el); else rowRefMap.current.delete(row.id); }}
                className={`group flex items-center border-b border-border/40 cursor-pointer ${isSelected ? "bg-accent/60" : "hover:bg-muted/30"}`}
                style={{ height: 22 }}
                onClick={() => setSelectedId(isSelected ? null : row.id)}
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
                    /* Stdio: same min-width bar */
                    <div
                      className={`absolute top-1/2 -translate-y-1/2 h-4 rounded-sm ${row.method === "err" ? "bg-red-400/70" : "bg-zinc-500/70"}`}
                      style={{ left: leftPct, width: "max(16px,0%)", maxWidth: `calc(100% - ${leftPct})` }}
                      title={row.label}
                    />
                  ) : (
                    /* Wrapper anchors both the bar and the label so the label
                       can sit outside the overflow-hidden bar div. */
                    <div
                      className="absolute top-1/2 -translate-y-1/2 h-4"
                      style={{ left: leftPct, width: `max(16px,${widthPct})`, maxWidth: `calc(100% - ${leftPct})` }}
                    >
                      {/* Bar visual — overflow-hidden clips the stripe animation */}
                      <div
                        className={`h-full w-full rounded-sm transition-none overflow-hidden relative ${isLive ? "bg-blue-500/50 border border-blue-400/60" : barClass(row)}`}
                        title={
                          isLive
                            ? `in-flight · ${dur}ms`
                            : row.pending
                              ? "no response received"
                              : `${row.durationMs}ms${row.status ? ` · ${row.status}` : ""}${row.error ? ` · ${row.error}` : ""}`
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
                        isPending={row.pending}
                        status={row.status}
                      />
                    </div>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      </div>
      <div ref={bottomRef} />
    </ScrollArea>

    {selectedRow && (
      panelCollapsed ? (
        <div className="shrink-0 border-t border-border flex items-center px-3 h-7">
          <button onClick={() => setPanelCollapsed(false)} className="text-muted-foreground/50 hover:text-muted-foreground transition-colors">
            <ChevronUp className="h-3.5 w-3.5" />
          </button>
        </div>
      ) : (
        <div className="h-[45%] shrink-0 border-t border-border flex flex-col overflow-hidden">
          <RowDetailPanel
            key={selectedRow.id}
            row={selectedRow}
            prevRow={prevAnthropicRow}
            onPrev={prevFilteredRow ? () => setSelectedId(prevFilteredRow.id) : undefined}
            onNext={nextFilteredRow ? () => setSelectedId(nextFilteredRow.id) : undefined}
            applyConfig={applyConfig}
          />
        </div>
      )
    )}
    </div>
  );
}
