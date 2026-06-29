import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ChevronDown, ChevronRight, ChevronUp } from "lucide-react";
import type { SandboxEvent, SandboxTarget } from "@/types";
import { humanDuration } from "@/lib/utils";
import { LLM_PROVIDERS } from "@/lib/llmProviders";
import { RowDetailPanel } from "./TimelineDetail";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

export interface TimelineBar {
  id: number;
  sandboxKey: string;
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
  type: "egress" | "ingress" | "fs" | "stdio" | "resource" | "exec";
  label: string;
  method?: string;
  isPoint: boolean;
  bars: TimelineBar[];
}

export function buildRows(events: SandboxEvent[], sandboxKey = ""): TimelineRow[] {
  const chunkMap = new Map<
    number,
    Extract<SandboxEvent, { type: "egress.chunk" }>[]
  >();
  const egressResMap = new Map<
    number,
    Extract<SandboxEvent, { type: "egress.response" }>
  >();
  const ingressResMap = new Map<
    number,
    Extract<SandboxEvent, { type: "ingress.response" }>
  >();
  const fsResMap = new Map<
    number,
    Extract<SandboxEvent, { type: "fs.response" }>
  >();
  const execResMap = new Map<
    number,
    Extract<SandboxEvent, { type: "exec.response" }>
  >();
  const rowMap = new Map<string, TimelineRow>();
  const rowOrder: string[] = [];

  for (const event of events) {
    if (event.type === "egress.chunk") {
      const list = chunkMap.get(event.request_id) ?? [];
      list.push(event);
      chunkMap.set(event.request_id, list);
    } else if (event.type === "egress.response") {
      egressResMap.set(event.request_id, event);
    } else if (event.type === "ingress.response") {
      ingressResMap.set(event.request_id, event);
    } else if (event.type === "fs.response") {
      fsResMap.set(event.request_id, event);
    } else if (event.type === "exec.response") {
      execResMap.set(event.request_id, event);
    }
  }

  function getOrCreateRow(
    key: string,
    type: TimelineRow["type"],
    label: string,
    method?: string,
    isPoint = false,
  ): TimelineRow {
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
        ? lastChunk
          ? new Date(lastChunk.timestamp).getTime() - startMs
          : res.duration_ms
        : 0;
      let label = event.host;
      let key: string;
      const matchedProvider = LLM_PROVIDERS.find((p) => p.matches(event));
      if (matchedProvider) {
        label = matchedProvider.extractLabel(event) ?? event.host;
        key = `llm:${label}`;
      } else {
        key = `egress:${event.host}`;
      }
      const row = getOrCreateRow(key, "egress", label);
      row.bars.push({
        id: event.id,
        sandboxKey,
        startTime: startMs,
        durationMs,
        status: res?.status,
        access: event.access,
        pending: !res,
        rawEvents: res ? [event, res, ...chunks] : [event, ...chunks],
      });
    } else if (event.type === "ingress.request") {
      const res = ingressResMap.get(event.id);
      const startMs = new Date(event.timestamp).getTime();
      const durationMs = res ? res.duration_ms : 0;
      const key = `ingress:${event.port}`;
      const row = getOrCreateRow(key, "ingress", `:${event.port}`);
      row.bars.push({
        id: event.id,
        sandboxKey,
        startTime: startMs,
        durationMs,
        status: res?.status,
        pending: !res,
        rawEvents: res ? [event, res] : [event],
      });
    } else if (event.type === "fs.request") {
      const res = fsResMap.get(event.id);
      const key = `fs:${event.mount}:${event.operation}`;
      const row = getOrCreateRow(key, "fs", event.mount, event.operation);
      row.bars.push({
        id: event.id,
        sandboxKey,
        startTime: new Date(event.timestamp).getTime(),
        durationMs: res?.duration_ms ?? 0,
        access: event.access,
        error: res?.error,
        pending: !res,
        rawEvents: res ? [event, res] : [event],
      });
    } else if (event.type === "stdio") {
      const method = event.stderr ? "err" : "out";
      const key = `stdio:${method}`;
      const row = getOrCreateRow(
        key,
        "stdio",
        method === "err" ? "stderr" : "stdout",
        method,
        false,
      );
      row.bars.push({
        id: event.id,
        sandboxKey,
        startTime: new Date(event.timestamp).getTime(),
        durationMs: 0,
        pending: false,
        rawEvents: [event],
      });
    } else if (event.type === "exec.request") {
      const res = execResMap.get(event.id);
      const startMs = new Date(event.timestamp).getTime();
      const durationMs = res ? new Date(res.timestamp).getTime() - startMs : 0;
      const truncCmd =
        event.command.length > 48
          ? event.command.slice(0, 48) + "…"
          : event.command;
      const row = getOrCreateRow(
        `exec:${event.id}`,
        "exec",
        truncCmd,
        "exec",
        false,
      );
      row.bars.push({
        id: event.id,
        sandboxKey,
        startTime: startMs,
        durationMs,
        pending: !res,
        rawEvents: res ? [event, res] : [event],
      });
    } else if (event.type === "resource.usage") {
      const t = new Date(event.timestamp).getTime();
      const bar = {
        id: event.id,
        sandboxKey,
        startTime: t,
        durationMs: 0,
        pending: false,
        rawEvents: [event],
      };
      getOrCreateRow(
        "resource:cpu",
        "resource",
        "cpu",
        undefined,
        true,
      ).bars.push(bar);
      getOrCreateRow(
        "resource:memory",
        "resource",
        "memory",
        undefined,
        true,
      ).bars.push({ ...bar });
    }
  }

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
    .map((k) => rowMap.get(k)!)
    .sort((a, b) => {
      const aTime =
        a.type === "fs"
          ? (mountEarliestTime.get(a.label) ?? 0)
          : (a.bars[0]?.startTime ?? 0);
      const bTime =
        b.type === "fs"
          ? (mountEarliestTime.get(b.label) ?? 0)
          : (b.bars[0]?.startTime ?? 0);
      if (aTime !== bTime) return aTime - bTime;
      if (a.type === "fs" && b.type === "fs" && a.label === b.label)
        return (a.method ?? "").localeCompare(b.method ?? "");
      return 0;
    });
}

const LANE_GAP_PX = 1;
const MIN_BAR_PX = 4;
const MIN_CLICK_TARGET_BAR_PX = 16;
const LANES_ENABLED = false;
const MERGE_OVERLAPS = true;
const VIRTUAL_SCROLL = false;
const GAP_THRESHOLD_MS = 60_000;

function computeLanes(
  bars: TimelineBar[],
  toDisplay: (t: number) => number,
  effectiveDurFn: (b: TimelineBar) => number,
): TimelineBar[][] {
  if (bars.length === 0) return [];
  const sorted = [...bars].sort((a, b) => a.startTime - b.startTime);
  const lanes: TimelineBar[][] = [];
  const laneRightPx: number[] = [];
  for (const bar of sorted) {
    const leftPx = toDisplay(bar.startTime);
    const rightPx = Math.max(
      leftPx + MIN_BAR_PX,
      toDisplay(bar.startTime + effectiveDurFn(bar)),
    );
    let placed = false;
    for (let i = 0; i < lanes.length; i++) {
      if (laneRightPx[i] + LANE_GAP_PX <= leftPx) {
        lanes[i].push(bar);
        laneRightPx[i] = rightPx;
        placed = true;
        break;
      }
    }
    if (!placed) {
      lanes.push([bar]);
      laneRightPx.push(rightPx);
    }
  }
  return lanes;
}

function isLiveBar(bar: TimelineBar): boolean {
  if (!bar.pending) return false;
  if (bar.access === "allowed") return true;
  return bar.rawEvents[0]?.type === "exec.request";
}

function liveBarClass(row: TimelineRow): string {
  if (row.type === "exec")
    return "bg-emerald-500/50 border border-emerald-400/60";
  if (row.type === "ingress")
    return "bg-blue-500/50 border border-blue-400/60";
  return "bg-blue-500/50 border border-blue-400/60";
}

function barClass(bar: TimelineBar, type: TimelineRow["type"]): string {
  if (bar.pending)
    return "bg-muted-foreground/40 border border-dashed border-muted-foreground/60";
  if (bar.access === "denied" || bar.error) return "bg-red-500/80";
  if (type === "egress") {
    return bar.status !== undefined && bar.status >= 400
      ? "bg-red-500/80"
      : "bg-blue-500/80";
  }
  if (type === "ingress") {
    return bar.status !== undefined && bar.status >= 400
      ? "bg-red-500/80"
      : "bg-blue-500/80";
  }
  if (type === "exec") return "bg-emerald-500/80";
  if (type === "stdio") {
    const ev = bar.rawEvents[0];
    return ev?.type === "stdio" && ev.stderr
      ? "bg-red-400/70"
      : "bg-zinc-500/70";
  }
  return "bg-purple-500/80";
}

function methodClass(row: TimelineRow): string {
  if (row.type === "stdio")
    return row.method === "err"
      ? "text-red-600 dark:text-red-400"
      : "text-muted-foreground";
  if (row.type === "exec") return "text-emerald-500 dark:text-emerald-400";
  if (row.type === "fs") return "text-purple-600 dark:text-purple-400";
  if (row.type === "resource")
    return row.key === "resource:cpu"
      ? "text-sky-600 dark:text-sky-400"
      : "text-emerald-600 dark:text-emerald-400";
  if (row.type === "ingress") return "text-blue-500 dark:text-blue-400";
  switch (row.method) {
    case "GET":
      return "text-green-600 dark:text-green-400";
    case "POST":
      return "text-foreground";
    case "PUT":
    case "PATCH":
      return "text-orange-600 dark:text-orange-400";
    case "DELETE":
      return "text-red-600 dark:text-red-400";
    default:
      return "text-muted-foreground";
  }
}

export type FilterKind = "all" | "egress" | "ingress" | "fs" | "llm";
export type FilterAccess = "all" | "allowed" | "denied";

export const KIND_OPTIONS: { value: FilterKind; label: string }[] = [
  { value: "all", label: "All" },
  { value: "egress", label: "Egress" },
  { value: "ingress", label: "Ingress" },
  { value: "fs", label: "File system" },
  { value: "llm", label: "LLM" },
];

export const ACCESS_OPTIONS: { value: FilterAccess; label: string }[] = [
  { value: "all", label: "Any" },
  { value: "allowed", label: "Allowed" },
  { value: "denied", label: "Denied" },
];

export interface FilterState {
  kind: FilterKind;
  access: FilterAccess;
  query: string;
}
export const EMPTY_FILTER: FilterState = {
  kind: "all",
  access: "all",
  query: "",
};

export function isFilterActive(f: FilterState) {
  return f.kind !== "all" || f.access !== "all" || f.query !== "";
}

export function applyFilter(
  rows: TimelineRow[],
  f: FilterState,
): TimelineRow[] {
  let out = rows;
  if (f.kind === "fs") out = out.filter((r) => r.type === "fs");
  else if (f.kind === "llm")
    out = out.filter((r) => {
      const e = r.bars[0]?.rawEvents[0];
      return (
        e?.type === "egress.request" && LLM_PROVIDERS.some((p) => p.matches(e))
      );
    });
  else if (f.kind === "egress") out = out.filter((r) => r.type === "egress");
  else if (f.kind === "ingress") out = out.filter((r) => r.type === "ingress");
  if (f.query) {
    const q = f.query.toLowerCase();
    out = out.filter((r) => r.label.toLowerCase().includes(q));
  }
  if (f.access === "allowed" || f.access === "denied") {
    out = out
      .map((r) => ({ ...r, bars: r.bars.filter((b) => b.access === f.access) }))
      .filter((r) => r.bars.length > 0);
  }
  return out;
}

export function filterEvents(
  events: SandboxEvent[],
  f: FilterState,
): SandboxEvent[] {
  let out = events;

  if (f.kind !== "all") {
    const llmIds =
      f.kind === "llm"
        ? new Set(
            events
              .filter(
                (e) =>
                  e.type === "egress.request" &&
                  LLM_PROVIDERS.some((p) => p.matches(e)),
              )
              .map((e) => e.id),
          )
        : null;
    out = out.filter((e) => {
      if (f.kind === "egress")
        return (
          e.type === "egress.request" ||
          e.type === "egress.response" ||
          e.type === "egress.chunk"
        );
      if (f.kind === "ingress")
        return e.type === "ingress.request" || e.type === "ingress.response";
      if (f.kind === "fs")
        return e.type === "fs.request" || e.type === "fs.response";
      if (f.kind === "llm") {
        if (e.type === "egress.request") return llmIds!.has(e.id);
        if (e.type === "egress.response" || e.type === "egress.chunk")
          return llmIds!.has(e.request_id);
        return false;
      }
      return true;
    });
  }

  if (f.access === "denied")
    out = out.filter((e) =>
      e.type === "egress.request" || e.type === "fs.request"
        ? e.access === "denied"
        : false,
    );
  else if (f.access === "allowed")
    out = out.filter((e) =>
      e.type === "egress.request" || e.type === "fs.request"
        ? e.access === "allowed"
        : true,
    );

  if (f.query) {
    const q = f.query.toLowerCase();
    out = out.filter((e) => {
      if (e.type === "egress.request")
        return `${e.host}${e.path}`.toLowerCase().includes(q);
      if (e.type === "ingress.request")
        return `${e.port}${e.path}`.toLowerCase().includes(q);
      if (e.type === "fs.request") return e.path.toLowerCase().includes(q);
      if (e.type === "stdio")
        return (e.stdout ?? e.stderr ?? "").toLowerCase().includes(q);
      if (e.type === "exec.request")
        return (
          e.command.toLowerCase().includes(q) || e.cwd.toLowerCase().includes(q)
        );
      return true;
    });
  }

  return out;
}

const DEFAULT_labelW = 220;

type Category = "llm" | "fs" | "egress" | "ingress" | "stdio" | "resource";

const CATEGORY_ORDER: Category[] = [
  "resource",
  "llm",
  "fs",
  "egress",
  "ingress",
  "stdio",
];
const CATEGORY_LABELS: Record<Category, string> = {
  llm: "LLM",
  fs: "File System",
  egress: "Egress",
  ingress: "Ingress",
  stdio: "Stdio",
  resource: "Resources",
};

function getRowCategory(row: TimelineRow): Category {
  if (row.type === "stdio" || row.type === "exec") return "stdio";
  if (row.type === "fs") return "fs";
  if (row.type === "resource") return "resource";
  if (row.type === "ingress") return "ingress";
  const e = row.bars[0]?.rawEvents[0];
  if (e?.type === "egress.request" && LLM_PROVIDERS.some((p) => p.matches(e))) {
    return "llm";
  }
  return "egress";
}

function getSubgroupKey(row: TimelineRow): string | null {
  if (row.type === "fs") return row.label;
  if (row.type === "egress") {
    const e = row.bars[0]?.rawEvents[0];
    return e?.type === "egress.request" ? e.host : null;
  }
  if (row.type === "ingress") {
    const e = row.bars[0]?.rawEvents[0];
    return e?.type === "ingress.request" ? e.port : null;
  }
  return null;
}

type DisplayItem =
  | { kind: "sandbox"; sandboxKey: string; allBars: TimelineBar[] }
  | { kind: "category"; category: Category; allBars: TimelineBar[] }
  | { kind: "row"; row: TimelineRow };

function buildDisplayItems(
  rowsByKey: ReadonlyMap<string, TimelineRow[]>,
  sandboxKeyOrder: readonly string[],
  collapsedCategories: ReadonlySet<string>,
  collapsedSandboxes: ReadonlySet<string>,
): DisplayItem[] {
  const items: DisplayItem[] = [];

  for (const sandboxKey of sandboxKeyOrder) {
    const rows = rowsByKey.get(sandboxKey);
    if (!rows || rows.length === 0) continue;

    items.push({ kind: "sandbox", sandboxKey, allBars: rows.flatMap((r) => r.bars) });
    if (collapsedSandboxes.has(sandboxKey)) continue;

    const byCategory = new Map<Category, TimelineRow[]>();
    for (const row of rows) {
      const cat = getRowCategory(row);
      const list = byCategory.get(cat);
      if (list) list.push(row);
      else byCategory.set(cat, [row]);
    }

    for (const cat of CATEGORY_ORDER) {
      const catRows = byCategory.get(cat);
      if (!catRows || catRows.length === 0) continue;

      items.push({ kind: "category", category: cat, allBars: catRows.flatMap((r) => r.bars) });
      if (collapsedCategories.has(`${sandboxKey}:${cat}`)) continue;

      if (cat === "fs" || cat === "egress" || cat === "ingress") {
        const bySubgroup = new Map<string, TimelineRow[]>();
        const subgroupOrder: string[] = [];
        for (const row of catRows) {
          const key = getSubgroupKey(row) ?? "";
          const existing = bySubgroup.get(key);
          if (existing) existing.push(row);
          else {
            bySubgroup.set(key, [row]);
            subgroupOrder.push(key);
          }
        }
        for (const key of subgroupOrder) {
          for (const row of bySubgroup.get(key)!)
            items.push({ kind: "row", row });
        }
      } else if (cat === "stdio") {
        const stdioOrder = (r: TimelineRow) =>
          r.type === "exec" ? 0 : r.method === "out" ? 1 : 2;
        for (const row of [...catRows].sort((a, b) => stdioOrder(a) - stdioOrder(b)))
          items.push({ kind: "row", row });
      } else {
        for (const row of catRows) items.push({ kind: "row", row });
      }
    }
  }

  return items;
}

// Virtual scroll types
interface VLane {
  row: TimelineRow;
  laneIdx: number;
  laneBars: TimelineBar[];
  lanes: TimelineBar[][];
  localTop: number; // offset within section rows area
  absoluteTop: number; // offset within full scroll container
}

interface VSection {
  category: Category;
  allBars: TimelineBar[];
  sandboxKey: string;
  collapsed: boolean;
  absoluteHeaderTop: number;
  absoluteRowsTop: number;
  laneCount: number;
  lanes: VLane[];
  totalHeight: number; // header (24) + laneCount * 22
}

interface VSandboxGroup {
  sandboxKey: string;
  allBars: TimelineBar[];
  collapsed: boolean;
  absoluteHeaderTop: number;
}

type RenderItem =
  | { kind: "sandbox-header"; group: VSandboxGroup }
  | { kind: "section"; section: VSection };

type ConfigUpdater = (cfg: Record<string, unknown>) => Record<string, unknown>;

function formatResourceBytes(bytes: number): string {
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
  if (bytes < 1024 * 1024 * 1024)
    return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)}GB`;
}

function ResourceLineChart({
  bars,
  rowKey,
  toDisplay,
  width,
  height,
  onSelect,
}: {
  bars: TimelineBar[];
  rowKey: string;
  toDisplay: (t: number) => number;
  width: number;
  height: number;
  onSelect?: (bar: TimelineBar) => void;
}) {
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);

  const isCPU = rowKey === "resource:cpu";
  const colors = isCPU
    ? { fill: "fill-sky-500/15", stroke: "stroke-sky-500/60" }
    : { fill: "fill-emerald-500/15", stroke: "stroke-emerald-500/60" };

  type Pt = { x: number; value: number; bar: TimelineBar };
  const pts: Pt[] = [];
  for (const bar of bars) {
    const ev = bar.rawEvents[0];
    if (ev?.type !== "resource.usage") continue;
    pts.push({
      x: toDisplay(bar.startTime),
      value: isCPU ? ev.cpu_percent : ev.memory_bytes,
      bar,
    });
  }
  if (pts.length === 0) return null;

  const maxValue = isCPU ? 100 : Math.max(...pts.map((p) => p.value), 1);
  const pad = 2;
  const chartH = height - pad * 2;
  const toY = (v: number) => pad + chartH * (1 - Math.min(v / maxValue, 1));

  const polyPoints = pts
    .map((p) => `${p.x.toFixed(1)},${toY(p.value).toFixed(1)}`)
    .join(" ");
  const areaD =
    pts.length > 1
      ? `M${pts[0].x.toFixed(1)},${toY(pts[0].value).toFixed(1)}` +
        pts
          .slice(1)
          .map((p) => ` L${p.x.toFixed(1)},${toY(p.value).toFixed(1)}`)
          .join("") +
        ` L${pts[pts.length - 1].x.toFixed(1)},${(height - pad).toFixed(1)}` +
        ` L${pts[0].x.toFixed(1)},${(height - pad).toFixed(1)} Z`
      : "";

  function onMouseMove(e: React.MouseEvent<SVGSVGElement>) {
    const mouseX = e.clientX - e.currentTarget.getBoundingClientRect().left;
    let best = 0,
      bestDist = Infinity;
    pts.forEach((p, i) => {
      const d = Math.abs(p.x - mouseX);
      if (d < bestDist) {
        bestDist = d;
        best = i;
      }
    });
    setHoverIdx(best);
  }

  const hov = hoverIdx !== null ? pts[hoverIdx] : null;
  const hovLabel = hov
    ? isCPU
      ? `${hov.value.toFixed(1)}%`
      : formatResourceBytes(hov.value)
    : null;

  function onClick(e: React.MouseEvent<SVGSVGElement>) {
    e.stopPropagation();
    const mouseX = e.clientX - e.currentTarget.getBoundingClientRect().left;
    let best = 0,
      bestDist = Infinity;
    pts.forEach((p, i) => {
      const d = Math.abs(p.x - mouseX);
      if (d < bestDist) {
        bestDist = d;
        best = i;
      }
    });
    if (pts[best]) onSelect?.(pts[best].bar);
  }

  return (
    <svg
      className="absolute inset-0 cursor-pointer"
      style={{ width, height }}
      onMouseMove={onMouseMove}
      onMouseLeave={() => setHoverIdx(null)}
      onClick={onClick}
    >
      {areaD && <path d={areaD} className={colors.fill} />}
      {pts.length > 1 && (
        <polyline
          points={polyPoints}
          className={`${colors.stroke} fill-none`}
          strokeWidth="1.5"
          strokeLinejoin="round"
        />
      )}
      {hov &&
        hovLabel &&
        (() => {
          const labelW = hovLabel.length * 5.5 + 8;
          const labelX = Math.max(
            labelW / 2,
            Math.min(hov.x, width - labelW / 2),
          );
          return (
            <>
              <line
                x1={hov.x}
                y1={0}
                x2={hov.x}
                y2={height}
                stroke="var(--chart-crosshair)"
                strokeWidth="1"
                strokeOpacity="1"
                strokeDasharray="2,2"
              />
              <rect
                x={labelX - labelW / 2}
                y={2}
                width={labelW}
                height={11}
                rx={2}
                fill="var(--chart-tooltip-bg)"
              />
              <text
                x={labelX}
                y={10.5}
                textAnchor="middle"
                fill="var(--chart-tooltip-text)"
                fontSize={8}
                fontFamily="ui-monospace,monospace"
              >
                {hovLabel}
              </text>
            </>
          );
        })()}
    </svg>
  );
}

function barKey(b: TimelineBar): string {
  return `${b.sandboxKey}:${b.rawEvents[0]?.type ?? ""}:${b.id}`;
}

export function TimelineView({
  events,
  filter,
  applyConfig,
  onOpenFile,
  zoomWindow,
  setZoomWindow,
  follow,
  onDisableFollow,
  detailInDialog = false,
}: {
  events: SandboxEvent[];
  filter: FilterState;
  applyConfig?: (
    updater: ConfigUpdater,
    target?: SandboxTarget,
  ) => Promise<void>;
  onOpenFile?: (path: string) => void;
  zoomWindow: { realStart: number; realEnd: number } | null;
  setZoomWindow: (w: { realStart: number; realEnd: number } | null) => void;
  follow?: boolean;
  onDisableFollow?: () => void;
  paused?: boolean;
  /** When true (single-column layout), selecting an event opens the detail in a
   *  dialog instead of the inline bottom panel, which has no room when stacked. */
  detailInDialog?: boolean;
}) {
  const sandboxKeyOrder = useMemo(() => {
    const seen = new Set<string>();
    const order: string[] = [];
    for (const e of events) {
      const k = e.sandbox_key ?? "";
      if (!seen.has(k)) { seen.add(k); order.push(k); }
    }
    return order;
  }, [events]);

  const rowsByKey = useMemo(() => {
    const byKey = new Map<string, SandboxEvent[]>();
    for (const e of events) {
      const k = e.sandbox_key ?? "";
      const list = byKey.get(k);
      if (list) list.push(e);
      else byKey.set(k, [e]);
    }
    const result = new Map<string, TimelineRow[]>();
    for (const [k, evs] of byKey) result.set(k, buildRows(evs, k));
    return result;
  }, [events]);

  const allRows = useMemo(() => [...rowsByKey.values()].flat(), [rowsByKey]);
  const [trackWidth, setTrackWidth] = useState(600);
  const [searchParams, setSearchParams] = useSearchParams();

  const [selectedId, setSelectedId] = useState<string | null>(() => {
    return searchParams.get("event");
  });
  const [panelCollapsed, setPanelCollapsed] = useState(
    () => localStorage.getItem("timeline:panelCollapsed") === "true",
  );
  // Full-screen detail dialog. Lives here (not inside RowDetailPanel) so it stays
  // open while the user pages through events with prev/next.
  const [detailExpanded, setDetailExpanded] = useState(false);

  useEffect(() => {
    localStorage.setItem("timeline:panelCollapsed", String(panelCollapsed));
  }, [panelCollapsed]);

  // Closing the detail panel (deselecting) also closes the expanded dialog.
  useEffect(() => {
    if (selectedId === null) setDetailExpanded(false);
  }, [selectedId]);

  const [panelHeight, setPanelHeight] = useState(() =>
    parseInt(localStorage.getItem("timeline:panelHeight") ?? "300", 10),
  );

  useEffect(() => {
    localStorage.setItem("timeline:panelHeight", String(panelHeight));
  }, [panelHeight]);

  const [labelW, setLabelW] = useState(() =>
    parseInt(
      localStorage.getItem("timeline:labelW") ?? String(DEFAULT_labelW),
      10,
    ),
  );
  const [labelDragging, setLabelDragging] = useState(false);

  useEffect(() => {
    localStorage.setItem("timeline:labelW", String(labelW));
  }, [labelW]);

  const [collapsedCategories, setCollapsedCategories] = useState<Set<string>>(
    () => {
      try {
        const saved = localStorage.getItem("timeline:collapsedCategories");
        return saved ? new Set(JSON.parse(saved) as string[]) : new Set();
      } catch {
        return new Set();
      }
    },
  );

  useEffect(() => {
    localStorage.setItem(
      "timeline:collapsedCategories",
      JSON.stringify([...collapsedCategories]),
    );
  }, [collapsedCategories]);

  const [collapsedSandboxes, setCollapsedSandboxes] = useState<Set<string>>(
    () => {
      try {
        const saved = localStorage.getItem("timeline:collapsedSandboxes");
        return saved ? new Set(JSON.parse(saved) as string[]) : new Set();
      } catch {
        return new Set();
      }
    },
  );

  useEffect(() => {
    localStorage.setItem(
      "timeline:collapsedSandboxes",
      JSON.stringify([...collapsedSandboxes]),
    );
  }, [collapsedSandboxes]);

  const [dragSel, setDragSel] = useState<{
    startPx: number;
    endPx: number;
  } | null>(null);
  const rulerDragRef = useRef<{ startPx: number } | null>(null);
  const dragHappenedRef = useRef(false);
  const targetScrollLeftRef = useRef<number>(0);

  useEffect(() => {
    if (!zoomWindow) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setZoomWindow(null);
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [zoomWindow]);

  useEffect(() => {
    if (zoomWindow == null) return;
    requestAnimationFrame(() => {
      const scrollTo = targetScrollLeftRef.current;
      if (rowsScrollRef.current) {
        rowsScrollRef.current.scrollLeft = scrollTo;
        setScrollLeft(scrollTo);
      }
      if (trackRef.current) trackRef.current.scrollLeft = scrollTo;
    });
  }, [zoomWindow]);

  // Virtual scroll state
  const [scrollTop, setScrollTop] = useState(0);
  const [scrollLeft, setScrollLeft] = useState(0);
  const [viewportHeight, setViewportHeight] = useState(400);
  const [viewportWidth, setViewportWidth] = useState(600);
  const rafScrollRef = useRef<number>(0);

  // Holds render-time computed values accessible from callbacks without stale closures
  const computedRef = useRef<{
    vsections: VSection[];
    realToDisplay: (t: number) => number;
    effectiveDur: (b: TimelineBar) => number;
    labelW: number;
    resourceStickyHeight: number;
  }>({
    vsections: [],
    realToDisplay: () => 0,
    effectiveDur: () => 0,
    labelW: DEFAULT_labelW,
    resourceStickyHeight: 0,
  });

  function toggleCategory(sandboxKey: string, cat: Category) {
    const k = `${sandboxKey}:${cat}`;
    setCollapsedCategories((prev) => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });
  }

  function toggleSandbox(key: string) {
    setCollapsedSandboxes((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  const containerRef = useRef<HTMLDivElement>(null);
  const trackRef = useRef<HTMLDivElement>(null);
  const rowsScrollRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const rowRefMap = useRef<Map<string, HTMLDivElement>>(new Map());

  useEffect(() => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (selectedId !== null) next.set("event", String(selectedId));
        else next.delete("event");
        return next;
      },
      { replace: true },
    );
  }, [selectedId]); // eslint-disable-line react-hooks/exhaustive-deps

  // Math-based scroll-into-view: reads vsections from computedRef to avoid stale closures
  const scrollSelectedIntoView = useCallback(() => {
    const scrollEl = rowsScrollRef.current;
    if (!scrollEl || selectedId === null) return;
    const {
      vsections,
      realToDisplay,
      effectiveDur,
      labelW: lw,
      resourceStickyHeight: stickyH,
    } = computedRef.current;

    for (const section of vsections) {
      for (const lane of section.lanes) {
        const bar = lane.laneBars.find((b) => barKey(b) === selectedId);
        if (!bar) continue;

        const barLeft = realToDisplay(bar.startTime);
        const barRight = Math.max(
          barLeft + 1,
          realToDisplay(bar.startTime + effectiveDur(bar)),
        );
        const laneTop = lane.absoluteTop;
        const laneBottom = laneTop + 22;

        const viewH = scrollEl.clientHeight;
        const viewW = scrollEl.clientWidth;
        const curScrollTop = scrollEl.scrollTop;
        const curScrollLeft = scrollEl.scrollLeft;

        // topCover: resource section is sticky at top=0; other sections have resource sticky + 24px category header
        const topCover = section.category === "resource" ? 0 : stickyH + 24;
        const trackW = viewW - lw;
        const hiddenV =
          laneTop < curScrollTop + topCover ||
          laneBottom > curScrollTop + viewH;
        const hiddenH =
          barLeft < curScrollLeft || barRight > curScrollLeft + trackW;

        if (!hiddenV && !hiddenH) return;

        if (hiddenV)
          scrollEl.scrollTop = Math.max(
            0,
            laneTop - topCover + 11 - (viewH - topCover) / 2,
          );
        if (hiddenH)
          scrollEl.scrollLeft = Math.max(
            0,
            (barLeft + barRight) / 2 - trackW / 2,
          );
        return;
      }
    }
  }, [selectedId]);

  useEffect(() => {
    if (selectedId === null) return;
    scrollSelectedIntoView();
  }, [selectedId, scrollSelectedIntoView]);

  // Observe the ruler track for responsive tick count
  useEffect(() => {
    const el = trackRef.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) =>
      setTrackWidth(entry.contentRect.width),
    );
    ro.observe(el);
    return () => ro.disconnect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allRows.length > 0]);

  // Observe rows viewport for virtual scroll dimensions.
  // Deps mirror trackRef: re-run when the scroll div first mounts (allRows.length > 0).
  // Read dimensions immediately so the first render uses the real size.
  useEffect(() => {
    const el = rowsScrollRef.current;
    if (!el) return;
    setViewportHeight(el.clientHeight);
    setViewportWidth(el.clientWidth);
    const ro = new ResizeObserver(([entry]) => {
      setViewportHeight(entry.contentRect.height);
      setViewportWidth(entry.contentRect.width);
    });
    ro.observe(el);
    return () => ro.disconnect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allRows.length > 0]);

  useEffect(() => {
    if (!follow) return;
    requestAnimationFrame(() => {
      const el = rowsScrollRef.current;
      if (el) el.scrollLeft = el.scrollWidth - el.clientWidth;
    });
  }, [follow, events.length]);

  const selectedBar =
    selectedId !== null
      ? (allRows.flatMap((r) => r.bars).find((b) => barKey(b) === selectedId) ?? null)
      : null;

  const llmBars = useMemo(
    () =>
      allRows
        .filter((r) => {
          const e = r.bars[0]?.rawEvents[0];
          return (
            e?.type === "egress.request" &&
            LLM_PROVIDERS.some((p) => p.matches(e))
          );
        })
        .flatMap((r) => r.bars),
    [allRows],
  );

  const prevAnthropicBar = (() => {
    const idx = llmBars.findIndex((b) => barKey(b) === selectedId);
    return idx > 0 ? llmBars[idx - 1] : null;
  })();

  if (allRows.length === 0) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        No events yet
      </div>
    );
  }

  // Per-sandbox-key filtering; preserve sandbox order.
  const filteredRowsByKey = new Map<string, TimelineRow[]>();
  for (const k of sandboxKeyOrder) {
    const rows = rowsByKey.get(k);
    if (!rows) continue;
    const filtered = applyFilter(rows, filter);
    if (filtered.length > 0) filteredRowsByKey.set(k, filtered);
  }
  const filteredSandboxKeyOrder = sandboxKeyOrder.filter((k) => filteredRowsByKey.has(k));

  if (filteredSandboxKeyOrder.length === 0) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        No events match the current filter.
      </div>
    );
  }

  const displayItems = buildDisplayItems(
    filteredRowsByKey,
    filteredSandboxKeyOrder,
    collapsedCategories,
    collapsedSandboxes,
  );

  const filteredBars = filteredSandboxKeyOrder.flatMap(
    (k) => filteredRowsByKey.get(k)!.flatMap((r) => r.bars),
  );
  const selectedBarIdx = filteredBars.findIndex((b) => barKey(b) === selectedId);
  const prevBarId =
    selectedBarIdx > 0 ? barKey(filteredBars[selectedBarIdx - 1]) : null;
  const nextBarId =
    selectedBarIdx >= 0 && selectedBarIdx < filteredBars.length - 1
      ? barKey(filteredBars[selectedBarIdx + 1])
      : null;

  const minTime = Math.min(...filteredBars.map((b) => b.startTime));
  const maxEventEnd = Math.max(
    ...filteredBars.map((b) => b.startTime + b.durationMs),
    minTime + 1,
  );
  const rightEdge = maxEventEnd;
  const rawSpan = Math.max(rightEdge - minTime, 1);
  const rightPad =
    trackWidth > 0 ? (30 / trackWidth) * rawSpan : rawSpan * 0.03;
  const totalSpan = rawSpan + rightPad;

  function effectiveDur(bar: TimelineBar): number {
    if (isLiveBar(bar)) {
      return Math.max(totalSpan - (bar.startTime - minTime), 0);
    }
    return bar.durationMs;
  }

  const pxPerMs = totalSpan > 0 && trackWidth > 0 ? trackWidth / totalSpan : 1;

  const rawIntervals: [number, number][] = [];
  for (const rows of filteredRowsByKey.values()) {
    for (const row of rows) {
      if (row.type === "resource") continue;
      for (const bar of row.bars) {
        rawIntervals.push([bar.startTime, bar.startTime + bar.durationMs]);
      }
    }
  }
  rawIntervals.sort((a, b) => a[0] - b[0]);
  const mergedIntervals: [number, number][] = [];
  for (const [s, e] of rawIntervals) {
    if (
      mergedIntervals.length === 0 ||
      s > mergedIntervals[mergedIntervals.length - 1][1]
    ) {
      mergedIntervals.push([s, e]);
    } else {
      mergedIntervals[mergedIntervals.length - 1][1] = Math.max(
        mergedIntervals[mergedIntervals.length - 1][1],
        e,
      );
    }
  }

  interface Segment {
    realStart: number;
    realEnd: number;
    dispStart: number;
    dispEnd: number;
    isGap: boolean;
  }
  const segments: Segment[] = [];
  let dispPos = 0;
  let prevEnd = minTime;
  for (const [iStart, iEnd] of mergedIntervals) {
    if (iStart > prevEnd) {
      const gapMs = iStart - prevEnd;
      const gapNatural = gapMs * pxPerMs;
      const isGap = gapMs >= GAP_THRESHOLD_MS;
      segments.push({
        realStart: prevEnd,
        realEnd: iStart,
        dispStart: dispPos,
        dispEnd: dispPos + gapNatural,
        isGap,
      });
      dispPos += gapNatural;
    }
    const evW = Math.max(1, (iEnd - iStart) * pxPerMs);
    segments.push({
      realStart: iStart,
      realEnd: iEnd,
      dispStart: dispPos,
      dispEnd: dispPos + evW,
      isGap: false,
    });
    dispPos += evW;
    prevEnd = Math.max(prevEnd, iEnd);
  }

  const fitScale =
    trackWidth > 0 && dispPos > 0 && dispPos + 10 < trackWidth
      ? (trackWidth - 10) / dispPos
      : 1;
  for (const seg of segments) {
    seg.dispStart *= fitScale;
    seg.dispEnd *= fitScale;
  }
  const contentTrackWidth =
    fitScale !== 1 ? trackWidth : Math.ceil(dispPos * fitScale + 10);

  function realToDisplay(realMs: number): number {
    for (const seg of segments) {
      if (realMs <= seg.realEnd) {
        const span = seg.realEnd - seg.realStart;
        return (
          seg.dispStart +
          (span > 0 ? (realMs - seg.realStart) / span : 0) *
            (seg.dispEnd - seg.dispStart)
        );
      }
    }
    return contentTrackWidth;
  }

  function displayToReal(dispPx: number): number {
    for (const seg of segments) {
      if (dispPx <= seg.dispEnd) {
        const span = seg.dispEnd - seg.dispStart;
        return (
          seg.realStart +
          (span > 0 ? (dispPx - seg.dispStart) / span : 0) *
            (seg.realEnd - seg.realStart)
        );
      }
    }
    return segments.length > 0
      ? segments[segments.length - 1].realEnd
      : minTime;
  }

  // ─── Zoom transform ───────────────────────────────────────────────────────
  let toDisplay: (t: number) => number;
  let fromDisplay: (px: number) => number;
  let effectiveTrackWidth: number;

  if (zoomWindow) {
    const zDispStart = realToDisplay(Math.max(minTime, zoomWindow.realStart));
    const zDispEnd = realToDisplay(Math.min(rightEdge, zoomWindow.realEnd));
    const zSpan = Math.max(1, zDispEnd - zDispStart);
    const scale = trackWidth / zSpan;
    toDisplay = (t: number) => realToDisplay(t) * scale;
    fromDisplay = (px: number) => displayToReal(px / scale);
    effectiveTrackWidth = Math.ceil(contentTrackWidth * scale);
    targetScrollLeftRef.current = zDispStart * scale;
  } else {
    toDisplay = realToDisplay;
    fromDisplay = displayToReal;
    effectiveTrackWidth = contentTrackWidth;
  }

  function startDrag(startPx: number, immediate: boolean) {
    rulerDragRef.current = { startPx };
    dragHappenedRef.current = false;
    let dragging = immediate;
    if (immediate) setDragSel({ startPx, endPx: startPx });

    function onMove(ev: MouseEvent) {
      if (!rulerDragRef.current || !trackRef.current) return;
      const r = trackRef.current.getBoundingClientRect();
      const curSl = rowsScrollRef.current?.scrollLeft ?? 0;
      const curPx = ev.clientX - r.left + curSl;
      if (!dragging && Math.abs(curPx - rulerDragRef.current.startPx) > 5) {
        dragging = true;
        dragHappenedRef.current = true;
        document.body.style.cursor = "crosshair";
        document.body.style.userSelect = "none";
      }
      if (dragging)
        setDragSel({ startPx: rulerDragRef.current.startPx, endPx: curPx });
    }

    function onUp(ev: MouseEvent) {
      if (!trackRef.current) return;
      if (dragging && rulerDragRef.current) {
        const r = trackRef.current.getBoundingClientRect();
        const curSl = rowsScrollRef.current?.scrollLeft ?? 0;
        const endPx = ev.clientX - r.left + curSl;
        const minPx = Math.min(rulerDragRef.current.startPx, endPx);
        const maxPx = Math.max(rulerDragRef.current.startPx, endPx);
        if (maxPx - minPx > 5) {
          setZoomWindow({
            realStart: fromDisplay(minPx),
            realEnd: fromDisplay(maxPx),
          });
        }
      }
      rulerDragRef.current = null;
      setDragSel(null);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseup", onUp);
      setTimeout(() => {
        dragHappenedRef.current = false;
      }, 0);
    }

    if (immediate) {
      document.body.style.cursor = "crosshair";
      document.body.style.userSelect = "none";
    }
    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
  }

  function onRulerMouseDown(e: React.MouseEvent) {
    if (e.button !== 0) return;
    const trackEl = trackRef.current;
    if (!trackEl) return;
    e.preventDefault();
    const rect = trackEl.getBoundingClientRect();
    const sl = rowsScrollRef.current?.scrollLeft ?? 0;
    startDrag(e.clientX - rect.left + sl, true);
  }

  function onRowsMouseDown(e: React.MouseEvent) {
    if (e.button !== 0) return;
    const trackEl = trackRef.current;
    if (!trackEl) return;
    const rect = trackEl.getBoundingClientRect();
    if (e.clientX < rect.left) return;
    e.preventDefault();
    const sl = rowsScrollRef.current?.scrollLeft ?? 0;
    startDrag(e.clientX - rect.left + sl, false);
  }

  const tickPositions = Array.from(
    { length: Math.floor(effectiveTrackWidth / 100) + 1 },
    (_, i) => i * 100,
  ).filter(
    (px) =>
      zoomWindow != null ||
      !segments.some((s) => s.isGap && px > s.dispStart && px < s.dispEnd),
  );

  // ─── Build virtual sections ───────────────────────────────────────────────

  const isMultiSandbox = filteredSandboxKeyOrder.length > 1;
  const vsections: VSection[] = [];
  const renderItems: RenderItem[] = [];
  let absTop = 0;
  let currentSandboxKey = "";

  for (const item of displayItems) {
    if (item.kind === "sandbox") {
      currentSandboxKey = item.sandboxKey;
      const group: VSandboxGroup = {
        sandboxKey: item.sandboxKey,
        allBars: item.allBars,
        collapsed: collapsedSandboxes.has(item.sandboxKey),
        absoluteHeaderTop: absTop,
      };
      renderItems.push({ kind: "sandbox-header", group });
      absTop += 28;
    } else if (item.kind === "category") {
      const collapsed = collapsedCategories.has(`${currentSandboxKey}:${item.category}`);
      const section: VSection = {
        sandboxKey: currentSandboxKey,
        category: item.category,
        allBars: item.allBars,
        collapsed,
        absoluteHeaderTop: absTop,
        absoluteRowsTop: absTop + 24,
        laneCount: 0,
        lanes: [],
        totalHeight: 0,
      };
      vsections.push(section);
      renderItems.push({ kind: "section", section });
      absTop += 24;
    } else {
      const section = vsections[vsections.length - 1];
      if (section && !section.collapsed) {
        if (item.row.type === "resource") {
          section.lanes.push({
            row: item.row,
            laneIdx: 0,
            laneBars: item.row.bars,
            lanes: [item.row.bars],
            localTop: section.laneCount * 22,
            absoluteTop: section.absoluteRowsTop + section.laneCount * 22,
          });
          section.laneCount++;
          absTop += 22;
        } else {
          const lanes = LANES_ENABLED
            ? computeLanes(item.row.bars, toDisplay, effectiveDur)
            : [[...item.row.bars].sort((a, b) => a.startTime - b.startTime)];
          for (let li = 0; li < lanes.length; li++) {
            section.lanes.push({
              row: item.row,
              laneIdx: li,
              laneBars: lanes[li],
              lanes,
              localTop: section.laneCount * 22,
              absoluteTop: section.absoluteRowsTop + section.laneCount * 22,
            });
            section.laneCount++;
            absTop += 22;
          }
        }
      }
    }
  }
  for (const s of vsections) {
    s.totalHeight = 24 + s.laneCount * 22;
  }

  // Resource section is only sticky in single-sandbox mode to avoid
  // multiple resource sections fighting to stick at top: 0.
  const resourceStickyHeight = !isMultiSandbox
    ? (vsections.find((s) => s.category === "resource")?.totalHeight ?? 0)
    : 0;

  // Update ref so callbacks can access current render values
  computedRef.current = {
    vsections,
    realToDisplay: toDisplay,
    effectiveDur,
    labelW,
    resourceStickyHeight,
  };

  // ─── Visibility windows ───────────────────────────────────────────────────

  const H_OVERSCAN = 200;
  const hVisLeft = scrollLeft - H_OVERSCAN;
  const hVisRight = scrollLeft + (viewportWidth - labelW) + H_OVERSCAN;

  const V_OVERSCAN_PX = 5 * 22;

  // Pre-filter ticks to visible horizontal window (shared by all rows)
  const visibleTicks = VIRTUAL_SCROLL
    ? tickPositions.filter((px) => px >= hVisLeft && px <= hVisRight)
    : tickPositions;

  // ─── Interaction handlers ─────────────────────────────────────────────────

  function onScroll() {
    const el = rowsScrollRef.current;
    if (trackRef.current && el) {
      trackRef.current.scrollLeft = el.scrollLeft;
      if (el.scrollLeft < el.scrollWidth - el.clientWidth - 1)
        onDisableFollow?.();
    }
    if (!VIRTUAL_SCROLL) return;
    if (rafScrollRef.current) cancelAnimationFrame(rafScrollRef.current);
    rafScrollRef.current = requestAnimationFrame(() => {
      const el = rowsScrollRef.current;
      if (!el) return;
      setScrollTop(el.scrollTop);
      setScrollLeft(el.scrollLeft);
    });
  }

  function startPanelDrag(e: React.MouseEvent) {
    e.preventDefault();
    const startY = e.clientY;
    const startHeight = panelHeight;
    const containerH =
      containerRef.current?.getBoundingClientRect().height ?? 600;
    document.body.style.cursor = "row-resize";
    document.body.style.userSelect = "none";
    function onMove(ev: MouseEvent) {
      const delta = startY - ev.clientY;
      setPanelHeight(
        Math.max(100, Math.min(startHeight + delta, containerH - 120)),
      );
    }
    function onUp() {
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseup", onUp);
    }
    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
  }

  function startLabelDrag(e: React.MouseEvent) {
    e.preventDefault();
    const startX = e.clientX;
    const startW = labelW;
    setLabelDragging(true);
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    function onMove(ev: MouseEvent) {
      setLabelW(Math.max(120, Math.min(startW + ev.clientX - startX, 480)));
    }
    function onUp() {
      setLabelDragging(false);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseup", onUp);
    }
    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
  }

  // ─── Render ───────────────────────────────────────────────────────────────

  return (
    <div ref={containerRef} className="flex flex-col h-full">
      <div className="relative flex flex-col min-h-0 flex-1">
        {/* Label column splitter */}
        <div
          className="absolute top-0 z-[31] w-[5px] cursor-col-resize group/lsplit"
          style={{ left: labelW - 2, bottom: 10 }}
          onMouseDown={startLabelDrag}
        >
          <div
            className={`absolute inset-y-0 left-[2px] w-px transition-colors ${labelDragging ? "bg-blue-700 dark:bg-blue-400" : "bg-border/40 group-hover/lsplit:bg-border"}`}
          />
        </div>

        {/* Sticky ruler */}
        <div className="shrink-0 text-xs cursor-default select-none border-b border-border">
          <div className="flex" style={{ paddingLeft: labelW }}>
            <div ref={trackRef} className="flex-1 overflow-hidden">
              <div
                className="group/ruler relative h-6"
                style={{ width: effectiveTrackWidth }}
                onMouseDown={onRulerMouseDown}
                onDoubleClick={() => setZoomWindow(null)}
              >
                {!zoomWindow &&
                  segments
                    .filter((s) => s.isGap)
                    .map((seg, i) => (
                      <div
                        key={`gap-${i}`}
                        className="gap-indicator group/gap absolute top-0 bottom-0 flex items-center justify-center border-x border-dashed border-zinc-500/30 bg-zinc-500/10 hover:bg-zinc-500/20 transition-colors"
                        style={{
                          left: seg.dispStart,
                          width: seg.dispEnd - seg.dispStart,
                        }}
                      >
                        <span className="text-[9px] text-muted-foreground whitespace-nowrap opacity-0 group-hover/gap:opacity-100 transition-opacity">
                          ~{humanDuration(seg.realEnd - seg.realStart)}
                        </span>
                      </div>
                    ))}
                {tickPositions.map((px, i) => {
                  const isFirst = i === 0;
                  const isLast = i === tickPositions.length - 1;
                  return (
                    <div
                      key={px}
                      className={`absolute top-0 flex flex-col transition-opacity group-has-[.gap-indicator:hover]/ruler:opacity-0 ${isLast ? "items-end" : isFirst ? "items-start" : "items-center"}`}
                      style={
                        isFirst
                          ? { left: 0 }
                          : isLast
                            ? { right: 0 }
                            : { left: px, transform: "translateX(-50%)" }
                      }
                    >
                      <span className="whitespace-nowrap text-muted-foreground">
                        {humanDuration(
                          fromDisplay(isLast ? effectiveTrackWidth : px) -
                            minTime,
                        )}
                      </span>
                      <div className="h-1.5 w-px bg-border" />
                    </div>
                  );
                })}
              </div>
            </div>
          </div>
        </div>

        {/* Rows — virtual scroll */}
        <div
          ref={rowsScrollRef}
          className="timeline-scroll scroll-container min-h-0 flex-1 overflow-auto text-xs cursor-default select-none"
          onScroll={onScroll}
          onMouseDown={onRowsMouseDown}
        >
          {/* Width wrapper — sections stack here in normal document flow */}
          <div style={{ width: labelW + effectiveTrackWidth }}>
            {renderItems.map((ritem) => {
              // ── Sandbox group header ──────────────────────────────────────
              if (ritem.kind === "sandbox-header") {
                const { group } = ritem;
                return (
                  <div
                    key={`sandbox:${group.sandboxKey}`}
                    style={{ height: 28 }}
                    className="flex border-y border-border bg-sidebar cursor-pointer"
                    onClick={() => toggleSandbox(group.sandboxKey)}
                  >
                    <div
                      className="shrink-0 sticky left-0 z-[22] flex items-center gap-1.5 px-2 bg-sidebar overflow-hidden"
                      style={{ width: labelW }}
                    >
                      {group.collapsed ? (
                        <ChevronRight className="h-3 w-3 shrink-0 text-muted-foreground" />
                      ) : (
                        <ChevronDown className="h-3 w-3 shrink-0 text-muted-foreground" />
                      )}
                      <span className="text-[11px] font-mono font-semibold text-foreground truncate tracking-tight">
                        {group.sandboxKey}
                      </span>
                    </div>
                    <div
                      className="relative self-stretch overflow-hidden"
                      style={{ width: effectiveTrackWidth }}
                    >
                      {visibleTicks.map((px) => (
                        <div key={px} className="absolute inset-y-0 w-px bg-border/30" style={{ left: px }} />
                      ))}
                      {group.collapsed && (() => {
                        const ranges = group.allBars
                          .map((b) => ({
                            left: toDisplay(b.startTime),
                            right: Math.max(toDisplay(b.startTime) + 5, toDisplay(b.startTime + effectiveDur(b))),
                            isLive: isLiveBar(b),
                          }))
                          .sort((a, b) => a.left - b.left);
                        const merged: { left: number; right: number; isLive: boolean }[] = [];
                        for (const r of ranges) {
                          const last = merged[merged.length - 1];
                          if (last && r.left <= last.right + 2) {
                            last.right = Math.max(last.right, r.right);
                            last.isLive = last.isLive || r.isLive;
                          } else merged.push({ ...r });
                        }
                        return merged
                          .filter((r) => r.right >= hVisLeft && r.left <= hVisRight)
                          .map((r, i) => (
                            <div
                              key={i}
                              className={`absolute top-1/2 -translate-y-1/2 h-3 rounded-sm overflow-hidden ${r.isLive ? "bg-muted-foreground/50 border border-muted-foreground/40" : "bg-muted-foreground/30"}`}
                              style={{ left: r.left, width: r.right - r.left }}
                            >
                              {r.isLive && <div className="absolute inset-0 bar-in-flight" />}
                            </div>
                          ));
                      })()}
                    </div>
                  </div>
                );
              }

              // ── Category section ──────────────────────────────────────────
              const { section } = ritem;
              const collapsed = section.collapsed;

              // Vertical: compute which lanes are in the visible window for this section.
              // Sticky sections are always visible regardless of scroll position.
              const localVisTop =
                scrollTop - section.absoluteRowsTop - V_OVERSCAN_PX;
              const localVisBottom =
                scrollTop +
                viewportHeight -
                section.absoluteRowsTop +
                V_OVERSCAN_PX;
              const visLanes =
                !VIRTUAL_SCROLL || section.category === "resource"
                  ? section.lanes
                  : section.lanes.filter(
                      (vl) =>
                        vl.localTop + 22 > localVisTop &&
                        vl.localTop < localVisBottom,
                    );

              const isResourceSection = !isMultiSandbox && section.category === "resource";
              return (
                // Section div has explicit height so sticky headers from later sections
                // push earlier ones out correctly via normal document flow.
                <div
                  key={`${section.sandboxKey}:${section.category}`}
                  style={{
                    height: section.totalHeight,
                    ...(isResourceSection ? { position: "sticky", top: 0, zIndex: 30 } : {}),
                  }}
                  className={isResourceSection ? "bg-background" : ""}
                >
                  {/* Category header — sticky within its section (single-sandbox only) */}
                  <div
                    className={`flex border-b border-border/60 bg-background cursor-pointer z-20${!isMultiSandbox ? " sticky" : ""}`}
                    style={{
                      height: 24,
                      top: !isMultiSandbox
                        ? (section.category === "resource" ? 0 : resourceStickyHeight)
                        : undefined,
                    }}
                    onClick={() => toggleCategory(section.sandboxKey, section.category)}
                  >
                    {/* Label cell — also sticky horizontally */}
                    <div
                      className="shrink-0 sticky left-0 z-[21] flex items-center gap-1.5 pl-5 pr-2 bg-background overflow-hidden"
                      style={{ width: labelW }}
                    >
                      {collapsed ? (
                        <ChevronRight className="h-3 w-3 shrink-0 text-muted-foreground" />
                      ) : (
                        <ChevronDown className="h-3 w-3 shrink-0 text-muted-foreground" />
                      )}
                      <span className="text-[10px] font-semibold uppercase tracking-wider text-foreground/70">
                        {CATEGORY_LABELS[section.category]}
                      </span>
                    </div>

                    {/* Track area */}
                    <div
                      className="relative self-stretch overflow-hidden"
                      style={{ width: effectiveTrackWidth }}
                    >
                      {visibleTicks.map((px) => (
                        <div
                          key={px}
                          className="absolute inset-y-0 w-px bg-border/30"
                          style={{ left: px }}
                        />
                      ))}
                      {collapsed &&
                        (() => {
                          const catColor = {
                            llm: "bg-blue-500/70",
                            egress: "bg-blue-500/70",
                            ingress: "bg-blue-500/70",
                            fs: "bg-purple-500/70",
                            stdio: "bg-zinc-500/70",
                            resource: "bg-emerald-500/70",
                          }[section.category];
                          const ranges = section.allBars
                            .map((b) => ({
                              left: toDisplay(b.startTime),
                              right: Math.max(
                                toDisplay(b.startTime) + 5,
                                toDisplay(b.startTime + effectiveDur(b)),
                              ),
                              isError:
                                b.access === "denied" ||
                                !!b.error ||
                                (b.status !== undefined && b.status >= 400),
                              isLive: isLiveBar(b),
                            }))
                            .sort((a, b) => a.left - b.left);
                          const merged: {
                            left: number;
                            right: number;
                            isError: boolean;
                            isLive: boolean;
                          }[] = [];
                          for (const r of ranges) {
                            const last = merged[merged.length - 1];
                            if (last && r.left <= last.right + 2) {
                              last.right = Math.max(last.right, r.right);
                              last.isError = last.isError || r.isError;
                              last.isLive = last.isLive || r.isLive;
                            } else merged.push({ ...r });
                          }
                          return merged
                            .filter(
                              (r) => r.right >= hVisLeft && r.left <= hVisRight,
                            )
                            .map((r, i) => (
                              <div
                                key={i}
                                className={`absolute top-1/2 -translate-y-1/2 h-3 rounded-sm overflow-hidden ${r.isError ? "bg-red-500/70" : r.isLive ? "bg-blue-500/50 border border-blue-400/60" : catColor}`}
                                style={{
                                  left: r.left,
                                  width: r.right - r.left,
                                }}
                              >
                                {r.isLive && (
                                  <div className="absolute inset-0 bar-in-flight" />
                                )}
                              </div>
                            ));
                        })()}
                    </div>
                  </div>

                  {/* Rows area — virtualized via absolute positioning */}
                  {!collapsed && (
                    <div
                      style={{
                        position: "relative",
                        height: section.laneCount * 22,
                      }}
                    >
                      {visLanes.map((vl) => {
                        const isResource = vl.row.type === "resource";
                        const isLaneSelected =
                          !isResource &&
                          vl.laneBars.some((b) => barKey(b) === selectedId);
                        return (
                          <div
                            key={`${vl.row.key}:${vl.laneIdx}`}
                            ref={
                              vl.laneIdx === 0
                                ? (el) => {
                                    if (el)
                                      rowRefMap.current.set(vl.row.key, el);
                                    else rowRefMap.current.delete(vl.row.key);
                                  }
                                : undefined
                            }
                            className={`group flex border-b border-border/40 ${isResource ? "cursor-default" : `cursor-pointer ${isLaneSelected ? "bg-indigo-100 dark:bg-[#1e1c50]" : "hover:bg-indigo-50 dark:hover:bg-[#16143a]"}`}`}
                            style={{
                              position: "absolute",
                              top: vl.localTop,
                              left: 0,
                              right: 0,
                              height: 22,
                            }}
                            onClick={() => {
                              if (isResource || dragHappenedRef.current) return;
                              const first = vl.laneBars[0];
                              if (!first) return;
                              const k = barKey(first);
                              setSelectedId(k === selectedId ? null : k);
                            }}
                          >
                            {/* Label cell — sticky horizontally */}
                            <div
                              className={`shrink-0 sticky left-0 z-10 flex items-center gap-1.5 overflow-hidden pl-8 pr-2 ${isLaneSelected ? "bg-indigo-100 dark:bg-[#1e1c50]" : isResource ? "bg-background" : "bg-background group-hover:bg-indigo-50 dark:group-hover:bg-[#16143a]"}`}
                              style={{ width: labelW }}
                            >
                              <span
                                className={`shrink-0 font-mono font-semibold ${methodClass(vl.row)}`}
                              >
                                {vl.row.method?.toUpperCase()}
                              </span>
                              <span
                                className={`truncate font-mono ${selectedId !== null && isLaneSelected ? "text-foreground" : "text-muted-foreground"}`}
                              >
                                {vl.row.label}
                              </span>
                            </div>

                            {/* Track cell — horizontal virtualization */}
                            <div
                              className="relative self-stretch overflow-hidden"
                              style={{ width: effectiveTrackWidth }}
                            >
                              {visibleTicks.map((px) => (
                                <div
                                  key={px}
                                  className="absolute inset-y-0 w-px bg-border/30"
                                  style={{ left: px }}
                                />
                              ))}
                              {vl.row.type === "resource" ? (
                                <ResourceLineChart
                                  bars={vl.row.bars}
                                  rowKey={vl.row.key}
                                  toDisplay={toDisplay}
                                  width={effectiveTrackWidth}
                                  height={22}
                                  onSelect={(bar) => {
                                    const k = barKey(bar);
                                    setSelectedId(k === selectedId ? null : k);
                                  }}
                                />
                              ) : (
                                (() => {
                                  const visible = !VIRTUAL_SCROLL
                                    ? vl.laneBars
                                    : vl.laneBars.filter((bar) => {
                                        const l = toDisplay(bar.startTime);
                                        const r = Math.max(
                                          l + 1,
                                          toDisplay(
                                            bar.startTime + effectiveDur(bar),
                                          ),
                                        );
                                        return r >= hVisLeft && l <= hVisRight;
                                      });
                                  if (vl.row.isPoint) {
                                    return visible.map((bar) => {
                                      const leftPx = toDisplay(bar.startTime);
                                      const isSelected = barKey(bar) === selectedId;
                                      return (
                                        <div
                                          key={bar.id}
                                          className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 rounded-sm ${vl.row.method === "err" ? "bg-red-400/70" : "bg-zinc-500/70"} ${!isSelected && selectedId !== null ? "opacity-50" : ""}`}
                                          style={{
                                            left: leftPx,
                                            width: MIN_BAR_PX,
                                            maxWidth: `calc(100% - ${leftPx}px)`,
                                          }}
                                          title={vl.row.label}
                                        >
                                          <div
                                            className="absolute inset-y-0 z-10 cursor-pointer"
                                            style={{
                                              left: "50%",
                                              transform: "translateX(-50%)",
                                              width: MIN_CLICK_TARGET_BAR_PX,
                                            }}
                                            onClick={(e) => {
                                              e.stopPropagation();
                                              if (dragHappenedRef.current)
                                                return;
                                              const k = barKey(bar);
                                              setSelectedId(k === selectedId ? null : k);
                                            }}
                                          />
                                        </div>
                                      );
                                    });
                                  }
                                  if (!MERGE_OVERLAPS) {
                                    return visible.map((bar) => {
                                      const leftPx = toDisplay(bar.startTime);
                                      const rightPx = Math.max(
                                        leftPx + MIN_BAR_PX,
                                        toDisplay(
                                          bar.startTime + effectiveDur(bar),
                                        ),
                                      );
                                      const isSelected = barKey(bar) === selectedId;
                                      const isLive = isLiveBar(bar);
                                      return (
                                        <div
                                          key={bar.id}
                                          className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 ${!isSelected && selectedId !== null ? "opacity-50" : ""}`}
                                          style={
                                            isLive
                                              ? { left: leftPx, right: 10 }
                                              : {
                                                  left: leftPx,
                                                  width: rightPx - leftPx,
                                                  maxWidth: `calc(100% - ${leftPx}px)`,
                                                }
                                          }
                                        >
                                          <div
                                            className={`h-full w-full rounded-sm transition-none overflow-hidden relative ${isLive ? liveBarClass(vl.row) : barClass(bar, vl.row.type)}`}
                                            title={
                                              isLive
                                                ? "in-flight"
                                                : bar.pending
                                                  ? "no response received"
                                                  : `${bar.durationMs}ms${bar.status ? ` · ${bar.status}` : ""}${bar.error ? ` · ${bar.error}` : ""}`
                                            }
                                          >
                                            {isLive && (
                                              <div className="absolute inset-0 bar-in-flight" />
                                            )}
                                          </div>
                                          <div
                                            className="absolute inset-y-0 z-10 cursor-pointer"
                                            style={
                                              isLive
                                                ? { inset: 0 }
                                                : {
                                                    left: "50%",
                                                    transform:
                                                      "translateX(-50%)",
                                                    width: Math.max(
                                                      rightPx - leftPx,
                                                      MIN_CLICK_TARGET_BAR_PX,
                                                    ),
                                                  }
                                            }
                                            onClick={(e) => {
                                              e.stopPropagation();
                                              if (dragHappenedRef.current)
                                                return;
                                              const k = barKey(bar);
                                              setSelectedId(k === selectedId ? null : k);
                                            }}
                                          />
                                        </div>
                                      );
                                    });
                                  }
                                  // Merge bars whose display footprints are within LANE_GAP_PX of each other.
                                  // rightPx includes MIN_BAR_PX so zero-duration bars have a footprint too.
                                  type Group = {
                                    bars: TimelineBar[];
                                    leftPx: number;
                                    rightPx: number;
                                  };
                                  const groups: Group[] = [];
                                  for (const bar of visible) {
                                    const l = toDisplay(bar.startTime);
                                    const r = Math.max(
                                      l + MIN_BAR_PX,
                                      toDisplay(
                                        bar.startTime + effectiveDur(bar),
                                      ),
                                    );
                                    const last = groups[groups.length - 1];
                                    if (
                                      last &&
                                      l < last.rightPx + LANE_GAP_PX
                                    ) {
                                      last.bars.push(bar);
                                      last.rightPx = Math.max(last.rightPx, r);
                                    } else {
                                      groups.push({
                                        bars: [bar],
                                        leftPx: l,
                                        rightPx: r,
                                      });
                                    }
                                  }
                                  return groups.map((group) => {
                                    const first = group.bars[0];
                                    const isSelected = group.bars.some(
                                      (b) => barKey(b) === selectedId,
                                    );
                                    const isLive = group.bars.some((b) =>
                                      isLiveBar(b),
                                    );
                                    const w = group.rightPx - group.leftPx;
                                    return (
                                      <div
                                        key={first.id}
                                        className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 ${!isSelected && selectedId !== null ? "opacity-50" : ""}`}
                                        style={
                                          isLive
                                            ? { left: group.leftPx, right: 10 }
                                            : {
                                                left: group.leftPx,
                                                width: w,
                                                maxWidth: `calc(100% - ${group.leftPx}px)`,
                                              }
                                        }
                                      >
                                        <div
                                          className={`h-full w-full rounded-sm transition-none overflow-hidden relative ${isLive ? liveBarClass(vl.row) : barClass(first, vl.row.type)}`}
                                          title={
                                            group.bars.length > 1
                                              ? `${group.bars.length} events`
                                              : isLive
                                                ? "in-flight"
                                                : first.pending
                                                  ? "no response received"
                                                  : `${first.durationMs}ms${first.status ? ` · ${first.status}` : ""}${first.error ? ` · ${first.error}` : ""}`
                                          }
                                        >
                                          {isLive && (
                                            <div className="absolute inset-0 bar-in-flight" />
                                          )}
                                        </div>
                                        <div
                                          className="absolute inset-y-0 z-10 cursor-pointer"
                                          style={
                                            isLive
                                              ? { inset: 0 }
                                              : {
                                                  left: "50%",
                                                  transform: "translateX(-50%)",
                                                  width: Math.max(
                                                    w,
                                                    MIN_CLICK_TARGET_BAR_PX,
                                                  ),
                                                }
                                          }
                                          onClick={(e) => {
                                            e.stopPropagation();
                                            if (dragHappenedRef.current) return;
                                            const k = barKey(first);
                                            setSelectedId(k === selectedId ? null : k);
                                          }}
                                        />
                                      </div>
                                    );
                                  });
                                })()
                              )}
                            </div>
                          </div>
                        );
                      })}
                    </div>
                  )}
                </div>
              );
            })}
            <div ref={bottomRef} />
          </div>
        </div>

        {dragSel &&
          (() => {
            const sl = rowsScrollRef.current?.scrollLeft ?? scrollLeft;
            const selLeft = Math.max(
              0,
              labelW + Math.min(dragSel.startPx, dragSel.endPx) - sl,
            );
            const selRight =
              labelW + Math.max(dragSel.startPx, dragSel.endPx) - sl;
            return (
              <>
                <div
                  className="pointer-events-none absolute top-0 bottom-0 bg-white/65 dark:bg-black/70 z-50"
                  style={{ left: 0, width: selLeft }}
                />
                <div
                  className="pointer-events-none absolute top-0 bottom-0 right-0 bg-white/65 dark:bg-black/70 z-50"
                  style={{ left: selRight }}
                />
                <div
                  className="pointer-events-none absolute top-0 bottom-0 w-px bg-blue-400/80 z-50"
                  style={{ left: selLeft }}
                />
                <div
                  className="pointer-events-none absolute top-0 bottom-0 w-px bg-blue-400/80 z-50"
                  style={{ left: selRight }}
                />
              </>
            );
          })()}
      </div>
      {/* end timeline area */}

      {selectedBar &&
        !detailInDialog &&
        (panelCollapsed ? (
          <div className="shrink-0 border-t border-border flex items-center px-3 h-7">
            <button
              onClick={() => setPanelCollapsed(false)}
              className="text-muted-foreground/50 hover:text-muted-foreground transition-colors"
            >
              <ChevronUp className="h-3.5 w-3.5" />
            </button>
          </div>
        ) : (
          <>
            <div
              className="h-[5px] shrink-0 cursor-row-resize bg-border hover:bg-foreground/20 transition-colors"
              onMouseDown={startPanelDrag}
            />
            <div
              className="shrink-0 flex flex-col overflow-hidden scroll-container"
              style={{ height: panelHeight }}
            >
              <RowDetailPanel
                key={selectedBar.id}
                bar={selectedBar}
                prevBar={prevAnthropicBar}
                onPrev={
                  prevBarId !== null
                    ? () => setSelectedId(prevBarId)
                    : undefined
                }
                onNext={
                  nextBarId !== null
                    ? () => setSelectedId(nextBarId)
                    : undefined
                }
                onExpand={() => setDetailExpanded(true)}
                applyConfig={applyConfig}
                onOpenFile={onOpenFile}
              />
            </div>
          </>
        ))}

      {selectedBar && (
        <Dialog
          open={detailInDialog ? true : detailExpanded}
          onOpenChange={(open) => {
            if (open) return;
            // In single-column mode the dialog *is* the selection, so closing it
            // deselects the event; otherwise it just collapses the expanded view.
            if (detailInDialog) setSelectedId(null);
            else setDetailExpanded(false);
          }}
        >
          <DialogContent className="max-w-7xl p-0 flex flex-col overflow-hidden h-[80vh]">
            <DialogTitle className="sr-only">Event detail</DialogTitle>
            <RowDetailPanel
              key={selectedBar.id}
              bar={selectedBar}
              prevBar={prevAnthropicBar}
              onPrev={
                prevBarId !== null ? () => setSelectedId(prevBarId) : undefined
              }
              onNext={
                nextBarId !== null ? () => setSelectedId(nextBarId) : undefined
              }
              applyConfig={applyConfig}
              onOpenFile={onOpenFile}
              expandedView
            />
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}
