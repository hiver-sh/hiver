import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ChevronDown, ChevronRight, Maximize2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  MosaicWithoutDragDropContext,
  type MosaicNode,
} from "react-mosaic-component";
import "react-mosaic-component/react-mosaic-component.css";
import type { SandboxEvent, SandboxTarget } from "@/types";
import { humanDuration } from "@/lib/utils";
import { useTransport } from "@/lib/transport";
import {
  LLM_PROVIDERS,
  extractLabelCached,
  matchProvider,
  parseSummaryCached,
} from "@/lib/llmProviders";
import type { LLMTool } from "@/lib/llmProviders";
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
  // Overrides the identity computed by barKey. Tool bars wrap the same LLM
  // egress events as their host LLM bar, so they need an explicit, distinct
  // key (keyed on the tool_use id) to avoid colliding with that LLM bar.
  keyOverride?: string;
  // Present on tool bars: the invocation extracted from an LLM tool_use →
  // tool_result round-trip. Drives the tool row's label and detail rendering.
  tool?: ToolInvocation;
}

export interface ToolInvocation {
  toolId: string;
  toolName: string;
  toolInput: unknown;
  toolResultContent?: unknown;
  /** The tool's declared schema, pulled from the LLM request that offered it. */
  definition?: LLMTool;
}

export interface TimelineRow {
  key: string;
  type:
    | "egress"
    | "ingress"
    | "fs"
    | "stdio"
    | "resource"
    | "exec"
    | "tool"
    | "system";
  label: string;
  method?: string;
  isPoint: boolean;
  bars: TimelineBar[];
  /**
   * Rows sharing a group sort as a block, positioned by the group's earliest
   * bar, so the several rows an fs mount or an HTTP authority now expands into
   * stay adjacent instead of scattering across the timeline by first-seen time.
   * Defaults to the row key, i.e. the row is its own group.
   */
  group?: string;
}

// Orders the fs operation rows within a mount: read, then write, then
// delete (destructive last). Unknown ops sort after, alphabetically.
function fsMethodRank(method?: string): number {
  switch (method) {
    case "read":
      return 0;
    case "write":
      return 1;
    case "delete":
      return 2;
    default:
      return 3;
  }
}

// The path component HTTP rows group on. Deliberately excludes the query
// string: it carries per-request tokens, cursors and timestamps, so folding it
// into the row key would put nearly every call on a row of its own. Paths
// themselves can still be high-cardinality when they embed ids (/users/123),
// which trades a denser timeline for one that distinguishes endpoints.
function routePath(path: string): string {
  if (!path) return "/";
  return path.startsWith("/") ? path : `/${path}`;
}

/**
 * Row identity for one HTTP request under the active grouping. `authority` is
 * the hostname for egress and `:port` for ingress. Only the selected fields
 * reach the key, so e.g. grouping on hostname alone folds every method and
 * path on a host back into a single row.
 */
function httpRowIdentity(
  prefix: "egress" | "ingress",
  groupBy: GroupByField[],
  authority: string,
  method: string,
  path: string,
): { key: string; label: string; method?: string; group?: string } {
  const on = (f: GroupByField) => groupBy.includes(f);
  const label =
    [on("host") ? authority : null, on("path") ? routePath(path) : null]
      .filter(Boolean)
      .join("") ||
    // Grouping on method alone leaves nothing host-shaped to name the row
    // after; the method itself renders in its own column beside this.
    "all requests";
  const key = [
    prefix,
    on("host") ? authority : "*",
    on("method") ? method : "*",
    on("path") ? routePath(path) : "*",
  ].join(":");
  return {
    key,
    label,
    // A row that aggregates several methods must not display one of them.
    method: on("method") ? method : undefined,
    // Keeping a host's rows adjacent only means anything while rows are split
    // within a host.
    group: on("host") ? `${prefix}:${authority}` : undefined,
  };
}

export function buildRows(
  events: SandboxEvent[],
  sandboxKey = "",
  groupByFields: GroupByField[] = DEFAULT_GROUP_BY,
): TimelineRow[] {
  const groupBy = normalizeGroupBy(groupByFields);
  const chunkMap = new Map<
    number,
    Extract<SandboxEvent, { type: "egress.chunk" | "ingress.chunk" }>[]
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
    if (event.type === "egress.chunk" || event.type === "ingress.chunk") {
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
    group?: string,
  ): TimelineRow {
    if (!rowMap.has(key)) {
      rowMap.set(key, { key, type, label, method, isPoint, bars: [], group });
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
      const matchedProvider = matchProvider(event);
      // LLM rows stay grouped by the model the provider extracts, not by route:
      // every call goes to the same endpoint, so routing them would collapse
      // them into one row and lose the per-model split.
      const id = matchedProvider
        ? (() => {
            const label = extractLabelCached(matchedProvider, event) ?? event.host;
            return { key: `llm:${label}`, label, method: undefined, group: undefined };
          })()
        : httpRowIdentity(
            "egress",
            groupBy,
            event.host,
            event.method,
            event.path,
          );
      const row = getOrCreateRow(
        id.key,
        "egress",
        id.label,
        id.method,
        false,
        id.group,
      );
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
      const chunks = chunkMap.get(event.id) ?? [];
      const startMs = new Date(event.timestamp).getTime();
      // ingress.response now carries time-to-first-byte; the body streams as
      // ingress.chunk events, so a streaming/WebSocket bar runs until its last
      // chunk. Fall back to the response's duration when there are no chunks.
      const lastChunk = chunks[chunks.length - 1];
      const durationMs = res
        ? lastChunk
          ? new Date(lastChunk.timestamp).getTime() - startMs
          : res.duration_ms
        : 0;
      const id = httpRowIdentity(
        "ingress",
        groupBy,
        `:${event.port}`,
        event.method,
        event.path,
      );
      const row = getOrCreateRow(
        id.key,
        "ingress",
        id.label,
        id.method,
        false,
        id.group,
      );
      row.bars.push({
        id: event.id,
        sandboxKey,
        startTime: startMs,
        durationMs,
        status: res?.status,
        pending: !res,
        rawEvents: res ? [event, res, ...chunks] : [event, ...chunks],
      });
    } else if (event.type === "fs.request") {
      const res = fsResMap.get(event.id);
      const key = `fs:${event.mount}:${event.operation}`;
      const row = getOrCreateRow(
        key,
        "fs",
        event.mount,
        event.operation,
        false,
        `fs:${event.mount}`,
      );
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
      // Group exec bars by working directory: one row per cwd, labeled with
      // the cwd, with each command an individual bar (its command is shown in
      // the detail panel on click). Mirrors how egress groups by host.
      const row = getOrCreateRow(
        `exec:${event.cwd}`,
        "exec",
        event.cwd,
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
    } else if (
      event.type === "system.start" ||
      event.type === "system.config-changed" ||
      event.type === "system.shutdown"
    ) {
      // Lifecycle transitions (start / config-changed / shutdown) render as
      // point markers on a single row grouped under Resources.
      getOrCreateRow("system", "system", "lifecycle", undefined, true).bars.push(
        {
          id: event.id,
          sandboxKey,
          startTime: new Date(event.timestamp).getTime(),
          durationMs: 0,
          pending: false,
          rawEvents: [event],
        },
      );
    }
  }

  // ─── Tools: correlate LLM tool_use → tool_result ─────────────────────────
  // A tool invocation spans from the moment the model emits a `tool_use` block
  // (the end of an LLM response — this is the "request" that starts the bar) to
  // the moment a later LLM request carries back the matching `tool_result` (the
  // "response" that ends the bar). Each invocation becomes one bar; bars are
  // grouped into rows by tool name, mirroring how egress groups by host.
  interface PendingTool {
    toolId: string;
    toolName: string;
    toolInput: unknown;
    startTime: number;
    endTime?: number;
    toolResultContent?: unknown;
    definition?: LLMTool;
    useEvents: SandboxEvent[];
    resultEvent?: SandboxEvent;
  }
  const toolsByStart = new Map<string, PendingTool>();
  const toolOrder: string[] = [];
  // Start times of every LLM request, in order — used by the fallback-close
  // pass below to complete tools whose tool_result we couldn't parse.
  const llmRequestTimes: number[] = [];

  for (const event of events) {
    if (event.type !== "egress.request") continue;
    const provider = matchProvider(event);
    if (!provider) continue;
    const res = egressResMap.get(event.id);
    const chunks = chunkMap.get(event.id) ?? [];
    const summary = parseSummaryCached(provider, event, res, chunks);
    if (!summary) continue;

    // tool_result blocks in this request's messages CLOSE earlier invocations.
    const reqTime = new Date(event.timestamp).getTime();
    llmRequestTimes.push(reqTime);
    for (const msg of summary.messages) {
      for (const blk of msg.content) {
        if (blk.type !== "tool_result" || !blk.toolId) continue;
        const t = toolsByStart.get(blk.toolId);
        if (t && t.endTime === undefined) {
          t.endTime = reqTime;
          t.toolResultContent = blk.toolResultContent;
          t.resultEvent = event;
        }
      }
    }

    // tool_use blocks in this request's response OPEN invocations. Their start
    // is the LLM response's completion time (last chunk, else response, else
    // request time) — that's when the model actually asked for the tool.
    const startMs = reqTime;
    const lastChunk = chunks[chunks.length - 1];
    const useStart = lastChunk
      ? new Date(lastChunk.timestamp).getTime()
      : res
        ? new Date(res.timestamp).getTime()
        : startMs;
    for (const blk of summary.response?.blocks ?? []) {
      if (blk.type !== "tool_use" || !blk.toolId) continue;
      if (toolsByStart.has(blk.toolId)) continue;
      const definition = summary.tools?.find((t) => t.name === blk.toolName);
      toolsByStart.set(blk.toolId, {
        toolId: blk.toolId,
        toolName: blk.toolName ?? "tool",
        toolInput: blk.toolInput,
        startTime: useStart,
        definition,
        useEvents: res ? [event, res, ...chunks] : [event, ...chunks],
      });
      toolOrder.push(blk.toolId);
    }
  }

  // Fallback close: a tool_use we couldn't pair with a tool_result — e.g. the
  // next request's body was truncated and didn't parse, so its tool_result
  // blocks were lost — but which is followed by another LLM request must have
  // completed before that request (the agent only continues once it has the
  // results). Close it at that next request's start. Tools from the final
  // response have no following request, so a genuinely in-flight tool stays
  // pending.
  for (const t of toolsByStart.values()) {
    if (t.endTime !== undefined) continue;
    const next = llmRequestTimes.find((rt) => rt > t.startTime);
    if (next !== undefined) t.endTime = next;
  }

  for (const toolId of toolOrder) {
    const t = toolsByStart.get(toolId)!;
    const row = getOrCreateRow(
      `tool:${t.toolName}`,
      "tool",
      t.toolName,
      undefined,
      false,
    );
    row.bars.push({
      id: t.useEvents[0]?.id ?? 0,
      sandboxKey,
      startTime: t.startTime,
      durationMs:
        t.endTime !== undefined ? Math.max(0, t.endTime - t.startTime) : 0,
      access: "allowed",
      pending: t.endTime === undefined,
      keyOverride: `tool:${sandboxKey}:${toolId}`,
      tool: {
        toolId: t.toolId,
        toolName: t.toolName,
        toolInput: t.toolInput,
        toolResultContent: t.toolResultContent,
        definition: t.definition,
      },
      rawEvents: t.resultEvent
        ? [...t.useEvents, t.resultEvent]
        : t.useEvents,
    });
  }

  // Earliest bar per group, so every row in a group sorts to the same slot and
  // the group lands where its first activity did.
  const groupEarliestTime = new Map<string, number>();
  for (const k of rowOrder) {
    const row = rowMap.get(k)!;
    const g = row.group ?? row.key;
    const t = row.bars[0]?.startTime ?? Infinity;
    const prev = groupEarliestTime.get(g) ?? Infinity;
    if (t < prev) groupEarliestTime.set(g, t);
  }

  // A group with no bars at all keeps the pre-existing "sorts first" behaviour
  // rather than being pushed to the end by the Infinity sentinel.
  const groupTime = (row: TimelineRow): number => {
    const t = groupEarliestTime.get(row.group ?? row.key);
    return t === undefined || t === Infinity ? 0 : t;
  };

  return rowOrder
    .map((k) => rowMap.get(k)!)
    .sort((a, b) => {
      const aTime = groupTime(a);
      const bTime = groupTime(b);
      if (aTime !== bTime) return aTime - bTime;
      if (a.group && a.group === b.group) {
        // Within an fs mount: read, then write, then delete. Other groups fall
        // through to insertion order (first-seen), which sort() preserves.
        if (a.type === "fs" && b.type === "fs")
          return fsMethodRank(a.method) - fsMethodRank(b.method);
      }
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
// Geometric step for quantizing the visible time span (see the span
// computation below). Larger values freeze the scale for longer between
// re-fits but reserve more empty space on the right (up to step−1 of the
// track); 1.25 caps the reserved space at ~20%.
const SPAN_STEP = 1.25;
// Width of the vertical scrollbar (see `::-webkit-scrollbar { width }` in
// index.css). Reserved on the right of the fit-mode track so the content
// wrapper stays a fixed `tile - gutter` wide regardless of whether the vertical
// scrollbar is showing. Without this the fit-mode wrapper equals the client
// width exactly, so a streaming event that tips content height past the viewport
// flips the vertical scrollbar on, which forces a horizontal scrollbar, which
// steals height, which flips the vertical scrollbar again — a per-frame thrash
// that makes the grid jitter ~1px while events stream.
const SCROLLBAR_GUTTER_PX = 10;

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

interface MergeGroup {
  bars: TimelineBar[];
  leftPx: number;
  rightPx: number;
}

// Fold bars whose display footprints are within LANE_GAP_PX of each other into
// merge groups (rightPx includes MIN_BAR_PX so zero-duration bars still have a
// footprint). Shared by the bar render and the row-click handler so a row click
// resolves to the exact same group the user sees under the cursor.
function computeMergeGroups(
  bars: TimelineBar[],
  toDisplay: (t: number) => number,
  effectiveDurFn: (b: TimelineBar) => number,
): MergeGroup[] {
  const groups: MergeGroup[] = [];
  for (const bar of bars) {
    const l = toDisplay(bar.startTime);
    const r = Math.max(
      l + MIN_BAR_PX,
      toDisplay(bar.startTime + effectiveDurFn(bar)),
    );
    const last = groups[groups.length - 1];
    if (last && l < last.rightPx + LANE_GAP_PX) {
      last.bars.push(bar);
      last.rightPx = Math.max(last.rightPx, r);
    } else {
      groups.push({ bars: [bar], leftPx: l, rightPx: r });
    }
  }
  return groups;
}

function liveBarClass(row: TimelineRow): string {
  if (row.type === "exec")
    return "bg-emerald-500/50 border border-emerald-400/60";
  if (row.type === "tool")
    return "bg-indigo-500/50 border border-indigo-400/60";
  if (row.type === "ingress")
    return "bg-blue-500/50 border border-blue-400/60";
  return "bg-blue-500/50 border border-blue-400/60";
}

function barClass(bar: TimelineBar, type: TimelineRow["type"]): string {
  if (bar.pending)
    return "bg-muted-foreground/40 border border-dashed border-muted-foreground/60";
  if (bar.access === "denied" || bar.error) return "bg-red-500/65";
  if (type === "egress") {
    return bar.status !== undefined && bar.status >= 400
      ? "bg-red-500/65"
      : "bg-blue-500/65";
  }
  if (type === "ingress") {
    return bar.status !== undefined && bar.status >= 400
      ? "bg-red-500/65"
      : "bg-blue-500/65";
  }
  if (type === "exec") return "bg-emerald-500/65";
  if (type === "tool") return "bg-indigo-500/65";
  if (type === "stdio") {
    const ev = bar.rawEvents[0];
    return ev?.type === "stdio" && ev.stderr
      ? "bg-red-400/70"
      : "bg-zinc-500/70";
  }
  if (type === "system") return "bg-amber-500/70";
  return "bg-purple-500/65";
}

function methodClass(row: TimelineRow): string {
  if (row.type === "stdio")
    return row.method === "err"
      ? "text-red-600 dark:text-red-400"
      : "text-muted-foreground";
  if (row.type === "exec") return "text-emerald-500 dark:text-emerald-400";
  if (row.type === "tool") return "text-indigo-600 dark:text-indigo-400";
  if (row.type === "fs") return "text-purple-600 dark:text-purple-400";
  if (row.type === "resource")
    return row.key === "resource:cpu"
      ? "text-sky-600 dark:text-sky-400"
      : "text-emerald-600 dark:text-emerald-400";
  if (row.type === "ingress") return "text-blue-500 dark:text-blue-400";
  if (row.type === "system") return "text-amber-600 dark:text-amber-400";
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

// The selectable kinds. "All" is not a member: it's the empty selection, so an
// unset filter and "everything selected" can't drift apart.
export type FilterKind = "egress" | "ingress" | "fs" | "llm" | "tools";
export type FilterAccess = "all" | "allowed" | "denied";

export const KIND_OPTIONS: { value: FilterKind; label: string }[] = [
  { value: "egress", label: "Egress" },
  { value: "ingress", label: "Ingress" },
  { value: "fs", label: "File system" },
  { value: "llm", label: "LLM" },
  { value: "tools", label: "Tools" },
];

export const ACCESS_OPTIONS: { value: FilterAccess; label: string }[] = [
  { value: "all", label: "Any" },
  { value: "allowed", label: "Allowed" },
  { value: "denied", label: "Denied" },
];

// Fields HTTP rows are grouped on. "host" is the request authority — the
// hostname for egress, the listening port for ingress.
export type GroupByField = "host" | "method" | "path";

export const GROUP_BY_OPTIONS: { value: GroupByField; label: string }[] = [
  { value: "host", label: "Hostname" },
  { value: "method", label: "Method" },
  { value: "path", label: "Path" },
];

export const DEFAULT_GROUP_BY: GroupByField[] = ["host"];

// Canonical ordering, so a row key doesn't depend on the order the user
// happened to click the toggles in.
const GROUP_BY_ORDER: GroupByField[] = ["host", "method", "path"];

export function normalizeGroupBy(fields: GroupByField[]): GroupByField[] {
  const set = new Set(fields);
  const out = GROUP_BY_ORDER.filter((f) => set.has(f));
  return out.length > 0 ? out : DEFAULT_GROUP_BY;
}

export interface FilterState {
  /** Selected kinds; empty means "all kinds", not "none". */
  kinds: FilterKind[];
  access: FilterAccess;
  query: string;
  groupBy: GroupByField[];
}
export const EMPTY_FILTER: FilterState = {
  kinds: [],
  access: "all",
  query: "",
  groupBy: DEFAULT_GROUP_BY,
};

// Grouping is a view preference, not a filter, so it deliberately doesn't count
// here: a non-default grouping shouldn't light up the filter button or make the
// toolbar render a "12 / 12 events" ratio.
export function isFilterActive(f: FilterState) {
  return f.kinds.length > 0 || f.access !== "all" || f.query !== "";
}

export function isGroupingCustom(f: FilterState) {
  const g = normalizeGroupBy(f.groupBy);
  return (
    g.length !== DEFAULT_GROUP_BY.length ||
    g.some((v, i) => v !== DEFAULT_GROUP_BY[i])
  );
}

export function applyFilter(
  rows: TimelineRow[],
  f: FilterState,
): TimelineRow[] {
  let out = rows;
  if (f.kinds.length > 0) {
    const kinds = new Set(f.kinds);
    const isLlmRow = (r: TimelineRow) => {
      if (r.type !== "egress") return false;
      const e = r.bars[0]?.rawEvents[0];
      return (
        e?.type === "egress.request" && LLM_PROVIDERS.some((p) => p.matches(e))
      );
    };
    // Kinds union rather than intersect. Note "egress" subsumes LLM rows, so
    // selecting Egress + LLM is the same set as Egress alone.
    out = out.filter(
      (r) =>
        (kinds.has("fs") && r.type === "fs") ||
        (kinds.has("tools") && r.type === "tool") ||
        (kinds.has("egress") && r.type === "egress") ||
        (kinds.has("ingress") && r.type === "ingress") ||
        (kinds.has("llm") && isLlmRow(r)),
    );
  }
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

  if (f.kinds.length > 0) {
    const kinds = new Set(f.kinds);
    // Tools are derived from the LLM egress traffic, so the raw feed shows the
    // same underlying LLM events for both the "llm" and "tools" kinds.
    const wantsLlm = kinds.has("llm") || kinds.has("tools");
    const llmIds = wantsLlm
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
      if (
        e.type === "egress.request" ||
        e.type === "egress.response" ||
        e.type === "egress.chunk"
      ) {
        if (kinds.has("egress")) return true;
        if (!wantsLlm) return false;
        return e.type === "egress.request"
          ? llmIds!.has(e.id)
          : llmIds!.has(e.request_id);
      }
      if (
        e.type === "ingress.request" ||
        e.type === "ingress.response" ||
        e.type === "ingress.chunk"
      )
        return kinds.has("ingress");
      if (e.type === "fs.request" || e.type === "fs.response")
        return kinds.has("fs");
      // Events that belong to no selectable kind (stdio, exec, resource,
      // system) stay visible only when nothing narrower is selected — which,
      // with a non-empty selection, means never.
      return false;
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

type Category =
  | "llm"
  | "tools"
  | "fs"
  | "egress"
  | "ingress"
  | "stdio"
  | "resource";

const CATEGORY_ORDER: Category[] = [
  "resource",
  "llm",
  "tools",
  "fs",
  "egress",
  "ingress",
  "stdio",
];
const CATEGORY_LABELS: Record<Category, string> = {
  llm: "LLM",
  tools: "Tools",
  fs: "File System",
  egress: "Egress",
  ingress: "Ingress",
  stdio: "Stdio",
  resource: "Resources",
};

function getRowCategory(row: TimelineRow): Category {
  if (row.type === "tool") return "tools";
  if (row.type === "stdio" || row.type === "exec") return "stdio";
  if (row.type === "fs") return "fs";
  if (row.type === "resource" || row.type === "system") return "resource";
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

  let maxValue = 1;
  if (isCPU) {
    maxValue = 100;
  } else {
    // Single pass; a long session's resource feed makes `pts` large enough that
    // Math.max(...spread) would overflow the call stack.
    for (const p of pts) if (p.value > maxValue) maxValue = p.value;
  }
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
  if (b.keyOverride) return b.keyOverride;
  return `${b.sandboxKey}:${b.rawEvents[0]?.type ?? ""}:${b.id}`;
}

// buildRows creates fresh bar objects on every run, but the SandboxEvents they
// wrap are immutable and referentially stable (the feed is append-only), so two
// bars with the same key, scalar fields, and rawEvents are the same content.
function barEquivalent(a: TimelineBar, b: TimelineBar): boolean {
  return (
    barKey(a) === barKey(b) &&
    a.startTime === b.startTime &&
    a.durationMs === b.durationMs &&
    a.status === b.status &&
    a.access === b.access &&
    a.error === b.error &&
    a.pending === b.pending &&
    a.rawEvents.length === b.rawEvents.length &&
    a.rawEvents[0] === b.rawEvents[0] &&
    a.rawEvents[a.rawEvents.length - 1] === b.rawEvents[b.rawEvents.length - 1]
  );
}

// Keeps a bar's object identity stable across renders while its content is
// unchanged, so the memoized RowDetailPanel doesn't re-render (and clear the
// user's text selection) every time a streamed event batch rebuilds the rows.
function useStableBar(candidate: TimelineBar | null): TimelineBar | null {
  const ref = useRef<TimelineBar | null>(null);
  const stable =
    candidate && ref.current && barEquivalent(ref.current, candidate)
      ? ref.current
      : candidate;
  ref.current = stable;
  return stable;
}

// Set-equality on bar keys, used to toggle a selected merge-group off when its
// bar is clicked again (group membership is order-independent).
function sameKeySet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  const s = new Set(a);
  return b.every((k) => s.has(k));
}

// Flatten a bar to the row shown in the grouped-events table.
function groupRowSummary(bar: TimelineBar): {
  time: string;
  method: string;
  primary: string;
  status: string;
  statusCls: string;
  duration: string;
} {
  const req = bar.rawEvents[0];
  const time = req ? new Date(req.timestamp).toISOString().slice(11, 23) : "";

  // Tool bars wrap LLM egress events, so rawEvents[0] is the invoking LLM
  // request — render the tool call itself (name / input / completion) instead
  // of that underlying POST /v1/messages.
  if (bar.tool) {
    const input =
      typeof bar.tool.toolInput === "string"
        ? bar.tool.toolInput
        : JSON.stringify(bar.tool.toolInput ?? {});
    return {
      time,
      method: bar.tool.toolName,
      primary: input.replace(/\s+/g, " ").trim(),
      status: bar.pending ? "pending" : "done",
      statusCls: bar.pending
        ? "text-muted-foreground/60"
        : "text-green-600 dark:text-green-400",
      duration: bar.durationMs > 0 ? humanDuration(bar.durationMs) : "",
    };
  }

  let method = "";
  let primary = "";
  if (req?.type === "egress.request" || req?.type === "ingress.request") {
    method = req.method;
    primary = req.query ? `${req.path}?${req.query}` : req.path;
  } else if (req?.type === "fs.request") {
    method = req.operation;
    primary = req.path;
  } else if (req?.type === "exec.request") {
    method = "exec";
    primary = req.command;
  } else if (req?.type === "stdio") {
    method = req.stderr ? "err" : "out";
    primary = (req.stdout ?? req.stderr ?? "").trimEnd();
  }

  // Status carries meaning for egress and fs (HTTP status / ACL decision / fs
  // error) and ingress (HTTP status of the served response); exec and stdio
  // leave the column blank. Ingress has no access/error fields, so it falls
  // through to the HTTP-status / pending branches below.
  let status = "";
  let statusCls = "text-muted-foreground";
  if (
    req?.type === "egress.request" ||
    req?.type === "fs.request" ||
    req?.type === "ingress.request"
  ) {
    if (bar.error) {
      status = bar.error;
      statusCls = "text-red-600 dark:text-red-400";
    } else if (bar.access === "denied") {
      status = "denied";
      statusCls = "text-red-600 dark:text-red-400";
    } else if (bar.status !== undefined) {
      status = String(bar.status);
      statusCls =
        bar.status >= 400
          ? "text-red-600 dark:text-red-400"
          : "text-green-600 dark:text-green-400";
    } else if (bar.pending) {
      status = "pending";
      statusCls = "text-muted-foreground/60";
    } else if (bar.access === "allowed") {
      status = "allowed";
      statusCls = "text-green-600 dark:text-green-400";
    }
  }

  // Duration is the request→response round-trip, meaningful for egress and
  // ingress.
  const duration =
    (req?.type === "egress.request" || req?.type === "ingress.request") &&
    bar.durationMs > 0
      ? humanDuration(bar.durationMs)
      : "";
  return { time, method, primary, status, statusCls, duration };
}

// Shown in the detail pane when a merged (multi-event) bar is selected: a table
// of every event folded into that bar. Clicking a row drills into that event's
// normal detail view (via onSelect → the timeline's selectedId).
function GroupEventsTable({
  bars,
  selectedId,
  onSelect,
  onExpand,
  expandedView,
}: {
  bars: TimelineBar[];
  selectedId: string | null;
  onSelect: (key: string) => void;
  onExpand?: () => void;
  expandedView?: boolean;
}) {
  const sorted = useMemo(
    () => [...bars].sort((a, b) => a.startTime - b.startTime),
    [bars],
  );
  // Every event in a merge group shares the same row (and thus type), so the
  // Status/Duration columns are either all-populated or all-empty. Drop them
  // when empty (e.g. stdio) so Detail gets the full width instead of leaving a
  // wasted gutter on the right.
  const reqType = bars[0]?.rawEvents[0]?.type;
  const showMethod =
    reqType === "egress.request" || reqType === "ingress.request";
  const showStatus =
    reqType === "egress.request" ||
    reqType === "fs.request" ||
    reqType === "ingress.request";
  const showDuration =
    reqType === "egress.request" || reqType === "ingress.request";
  return (
    <div className="flex flex-col h-full text-xs">
      <div className="relative shrink-0">
        <div className="flex items-center gap-2 px-3 py-2">
          <span className="text-[10px] uppercase tracking-wider text-muted-foreground font-semibold">
            {bars.length} events
          </span>
          {!expandedView && onExpand && (
            <Button
              size="sm"
              variant="ghost"
              className="ml-auto"
              onClick={onExpand}
              title="Expand"
            >
              <Maximize2 className="h-3.5 w-3.5" />
            </Button>
          )}
        </div>
        <div className="pointer-events-none absolute inset-y-0 right-0 w-8 bg-gradient-to-l from-background to-transparent" />
      </div>
      <div className="flex-1 min-h-0 overflow-auto mx-3 mb-3 rounded-md border border-border">
        <table className="w-full table-fixed border-collapse font-mono">
          <colgroup>
            <col className="w-[104px]" />
            {showMethod && <col className="w-[64px]" />}
            <col />
            {showStatus && <col className="w-[72px]" />}
            {showDuration && <col className="w-[80px]" />}
          </colgroup>
          <thead className="sticky top-0 z-10 bg-background">
            <tr className="text-left text-[10px] uppercase tracking-wider text-muted-foreground/60">
              <th className="px-2 py-1.5 font-medium">Time</th>
              {showMethod && <th className="px-2 py-1.5 font-medium">Method</th>}
              <th className="px-2 py-1.5 font-medium">Detail</th>
              {showStatus && (
                <th className="px-2 py-1.5 font-medium">Status</th>
              )}
              {showDuration && (
                <th className="px-2 py-1.5 font-medium text-right">Duration</th>
              )}
            </tr>
          </thead>
          <tbody>
            {sorted.map((bar) => {
              const s = groupRowSummary(bar);
              const key = barKey(bar);
              const isSel = key === selectedId;
              return (
                <tr
                  key={key}
                  onClick={() => onSelect(key)}
                  className={`cursor-pointer border-t border-border/40 ${isSel ? "bg-indigo-100 dark:bg-[#1e1c50]" : "hover:bg-indigo-50 dark:hover:bg-[#16143a]"}`}
                >
                  <td className="px-2 py-1 text-muted-foreground whitespace-nowrap align-top">
                    {s.time}
                  </td>
                  {showMethod && (
                    <td className="px-2 py-1 text-muted-foreground truncate align-top">
                      {s.method}
                    </td>
                  )}
                  <td className="px-2 py-1 truncate align-top" title={s.primary}>
                    {s.primary}
                  </td>
                  {showStatus && (
                    <td
                      className={`px-2 py-1 truncate align-top ${s.statusCls}`}
                      title={s.status}
                    >
                      {s.status}
                    </td>
                  )}
                  {showDuration && (
                    <td className="px-2 py-1 text-right text-muted-foreground whitespace-nowrap align-top">
                      {s.duration}
                    </td>
                  )}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// The two tiles of the timeline's internal mosaic split: the event rows and the
// selected-event detail panel.
type TimelinePane = "rows" | "detail";

function TimelineViewInner({
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
    for (const [k, evs] of byKey) result.set(k, buildRows(evs, k, filter.groupBy));
    return result;
  }, [events, filter.groupBy]);

  const allRows = useMemo(() => [...rowsByKey.values()].flat(), [rowsByKey]);

  // Latest timestamp across all events — the timeline's notion of "now".
  // Live (in-flight) bars are sized against this, NOT the wall clock: for
  // recorded/replayed traces the timestamps are historical, and wall-clock
  // now would stretch the span across the whole gap between the recording
  // and today, crushing the trace into a sliver. For live sessions the two
  // are equivalent, since renders only happen when events arrive anyway.
  const latestEventMs = useMemo(() => {
    let max = 0;
    for (const e of events) {
      const t = new Date(e.timestamp).getTime();
      if (t > max) max = t;
    }
    return max;
  }, [events]);

  // "Now" for sizing in-flight bars, in event-timestamp coordinates. Anchored
  // at the latest event and advanced by the *transport's* clock: the trace
  // player's speed-scaled replay clock when replaying, the wall clock for
  // live sessions. Time in a trace is mocked — it runs at TransportProvider's
  // `speed`, can be re-paced or paused, and its event timestamps are
  // historical — so the browser clock must never be compared against event
  // timestamps directly.
  const { player } = useTransport();
  const tickerNow = player ? player.elapsedReplayMs : Date.now();
  const liveAnchorRef = useRef({ eventAbs: 0, ticker: 0 });
  if (
    liveAnchorRef.current.eventAbs === 0 ||
    latestEventMs < liveAnchorRef.current.eventAbs
  ) {
    // First events, or the event list was cleared/reloaded: (re)anchor.
    liveAnchorRef.current = { eventAbs: latestEventMs, ticker: tickerNow };
  } else if (latestEventMs > liveAnchorRef.current.eventAbs) {
    // New event: re-anchor at the later of the event time and our running
    // clock, so "now" never steps backward on delivery jitter.
    const anchoredNow =
      liveAnchorRef.current.eventAbs +
      Math.max(0, tickerNow - liveAnchorRef.current.ticker);
    liveAnchorRef.current = {
      eventAbs: Math.max(latestEventMs, anchoredNow),
      ticker: tickerNow,
    };
  }
  const liveNowMs =
    liveAnchorRef.current.eventAbs +
    Math.max(0, tickerNow - liveAnchorRef.current.ticker);

  // Re-render on a slow tick while anything is in flight, so live bars keep
  // growing between event arrivals (renders otherwise only happen when events
  // arrive). Skipped once live bars are capped at the span end — a recorded
  // trace with a dangling in-flight request would tick forever otherwise.
  const hasLiveBars = allRows.some((r) => r.bars.some(isLiveBar));
  const [, setLiveTick] = useState(0);
  const [trackWidth, setTrackWidth] = useState(600);
  const [searchParams, setSearchParams] = useSearchParams();

  const [selectedId, setSelectedId] = useState<string | null>(() => {
    return searchParams.get("event");
  });
  // When a merged (multi-event) bar is selected, the bar keys of every event
  // folded into it. The detail pane then shows the grouped-events table instead
  // of a single event's detail. Mutually exclusive with a non-null selectedId:
  // drilling into a table row sets selectedId (see the effect below).
  const [selectedGroupKeys, setSelectedGroupKeys] = useState<string[] | null>(
    null,
  );
  // Full-screen detail dialog. Lives here (not inside RowDetailPanel) so it stays
  // open while the user pages through events with prev/next.
  const [detailExpanded, setDetailExpanded] = useState(false);

  // Selecting an individual event closes the grouped-events table (drilling in).
  useEffect(() => {
    if (selectedId !== null) setSelectedGroupKeys(null);
  }, [selectedId]);

  // Closing the detail pane (nothing selected) also closes the expanded dialog.
  useEffect(() => {
    if (selectedId === null && selectedGroupKeys === null)
      setDetailExpanded(false);
  }, [selectedId, selectedGroupKeys]);

  // Rows/detail split is resized by the mosaic splitter (a single, unified resize
  // mechanism across the whole inspector). `detailSplit` is the percentage of the
  // vertical space given to the timeline rows; the detail panel takes the rest.
  const [detailSplit, setDetailSplit] = useState(() => {
    const s = parseFloat(localStorage.getItem("timeline:detailSplit") ?? "");
    return Number.isFinite(s) ? s : 62;
  });

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
    liveCapped: boolean;
  }>({
    vsections: [],
    realToDisplay: () => 0,
    effectiveDur: () => 0,
    labelW: DEFAULT_labelW,
    resourceStickyHeight: 0,
    liveCapped: false,
  });

  useEffect(() => {
    if (!hasLiveBars) return;
    const id = setInterval(() => {
      if (!computedRef.current.liveCapped) setLiveTick((n) => n + 1);
    }, 250);
    return () => clearInterval(id);
  }, [hasLiveBars]);

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

  // Observe the ruler track for responsive tick count.
  // The callback is deferred to a rAF and only updates on a real (>0.5px) change:
  // when the panel is very small, scrollbars flip on/off each layout pass and a
  // synchronous ResizeObserver → setState → relayout cycle would spin the CPU.
  useEffect(() => {
    const el = trackRef.current;
    if (!el) return;
    let raf = 0;
    const ro = new ResizeObserver((entries) => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        const w = entries[0]?.contentRect.width;
        if (!Number.isFinite(w)) return;
        setTrackWidth((prev) => (Math.abs(prev - w) > 0.5 ? w : prev));
      });
    });
    ro.observe(el);
    return () => {
      cancelAnimationFrame(raf);
      ro.disconnect();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allRows.length > 0]);

  // Observe rows viewport for virtual scroll dimensions. Same rAF + change-guard
  // as above to stay stable when the panel is tiny. Read dimensions immediately
  // so the first render uses the real size.
  useEffect(() => {
    const el = rowsScrollRef.current;
    if (!el) return;
    setViewportHeight(el.clientHeight);
    setViewportWidth(el.clientWidth);
    let raf = 0;
    const ro = new ResizeObserver((entries) => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        const cr = entries[0]?.contentRect;
        if (!cr) return;
        if (Number.isFinite(cr.height))
          setViewportHeight((h) => (Math.abs(h - cr.height) > 0.5 ? cr.height : h));
        if (Number.isFinite(cr.width))
          setViewportWidth((w) => (Math.abs(w - cr.width) > 0.5 ? cr.width : w));
      });
    });
    ro.observe(el);
    return () => {
      cancelAnimationFrame(raf);
      ro.disconnect();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allRows.length > 0]);

  useEffect(() => {
    if (!follow) return;
    requestAnimationFrame(() => {
      const el = rowsScrollRef.current;
      if (el) el.scrollLeft = el.scrollWidth - el.clientWidth;
    });
  }, [follow, events.length]);

  // Identity-stabilized (useStableBar) so the memoized RowDetailPanel skips
  // re-rendering — and the user's text selection inside it survives — while
  // streamed events rebuild the rows around an unchanged selection.
  const selectedBar = useStableBar(
    selectedId !== null
      ? (allRows.flatMap((r) => r.bars).find((b) => barKey(b) === selectedId) ?? null)
      : null,
  );

  // Resolve the selected merge-group's bar keys back to (freshly rebuilt) bars.
  // Bars are recreated every render, so we key off the stable barKey. Null when
  // nothing groupable is selected or the group's events were filtered away.
  const selectedGroupBars = useMemo(() => {
    if (selectedGroupKeys === null) return null;
    const byKey = new Map<string, TimelineBar>();
    for (const r of allRows) for (const b of r.bars) byKey.set(barKey(b), b);
    const bars = selectedGroupKeys
      .map((k) => byKey.get(k))
      .filter((b): b is TimelineBar => !!b);
    return bars.length > 0 ? bars : null;
  }, [selectedGroupKeys, allRows]);

  const llmBars = useMemo(
    () =>
      allRows
        .filter((r) => {
          if (r.type !== "egress") return false;
          const e = r.bars[0]?.rawEvents[0];
          return (
            e?.type === "egress.request" &&
            LLM_PROVIDERS.some((p) => p.matches(e))
          );
        })
        .flatMap((r) => r.bars),
    [allRows],
  );

  const prevAnthropicBar = useStableBar(
    (() => {
      const idx = llmBars.findIndex((b) => barKey(b) === selectedId);
      return idx > 0 ? llmBars[idx - 1] : null;
    })(),
  );

  // Stable nav/expand handler identities for the memoized RowDetailPanel; the
  // prev/next targets are computed further down (after the early returns) and
  // read through refs at click time.
  const prevBarIdRef = useRef<string | null>(null);
  const nextBarIdRef = useRef<string | null>(null);
  const selectPrevBar = useCallback(() => {
    if (prevBarIdRef.current !== null) setSelectedId(prevBarIdRef.current);
  }, []);
  const selectNextBar = useCallback(() => {
    if (nextBarIdRef.current !== null) setSelectedId(nextBarIdRef.current);
  }, []);
  const expandDetail = useCallback(() => setDetailExpanded(true), []);

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
  // Feed the stable nav handlers (see selectPrevBar/selectNextBar above).
  prevBarIdRef.current = prevBarId;
  nextBarIdRef.current = nextBarId;

  // The horizontal time→px scale is anchored to *all* bars, not the filtered
  // subset, so applying/clearing a filter only hides rows — it never rescales
  // or shifts the bars that remain visible. (Nav still uses filteredBars below.)
  const scaleBars = allRows.flatMap((r) => r.bars);
  // Single pass rather than Math.min/max(...spread): spreading every bar as
  // call args allocates two throwaway arrays each render and overflows the call
  // stack once a session accumulates enough bars.
  let minTime = Infinity;
  let maxEventEnd = -Infinity;
  for (const b of scaleBars) {
    if (b.startTime < minTime) minTime = b.startTime;
    const end = b.startTime + b.durationMs;
    if (end > maxEventEnd) maxEventEnd = end;
  }
  if (!Number.isFinite(minTime)) minTime = 0;
  maxEventEnd = Math.max(maxEventEnd, minTime + 1);
  // Extend the timeline to the latest event timestamp so live bars have room
  // to be drawn at their elapsed-so-far width (chunk/resource events advance
  // it past maxEventEnd, which only counts completed bar durations). The span
  // quantization below keeps the scale frozen as it advances.
  const rightEdge = Math.max(maxEventEnd, latestEventMs);
  const rawSpan = Math.max(rightEdge - minTime, 1);
  // Quantize the span up to a geometric step so the time→px scale stays frozen
  // while events stream in. Recomputing the scale on every append made the
  // whole grid self-adjust (every bar compressing a hair per event) and let
  // the content width oscillate around the viewport width, flipping the
  // horizontal scrollbar on/off — each flip steals/returns the scrollbar's
  // height and reflows the grid. With a quantized span the mapping only
  // changes when the session outgrows the current step (~every 25% of
  // growth); between steps, new events render into already-reserved space
  // and nothing moves.
  const span = Math.pow(
    SPAN_STEP,
    Math.ceil(Math.log(rawSpan) / Math.log(SPAN_STEP)),
  );
  const spanEnd = minTime + span;
  const rightPad = trackWidth > 0 ? (30 / trackWidth) * span : span * 0.03;
  const totalSpan = span + rightPad;

  // Live (in-flight egress/fs/exec) bars are drawn at their elapsed-so-far
  // width: they grow as the (transport) clock advances and freeze in place
  // when the response lands. Clamped to the reserved span end so a dangling
  // in-flight request can never stretch the scale on its own.
  function effectiveDur(bar: TimelineBar): number {
    if (isLiveBar(bar)) {
      return Math.max(Math.min(liveNowMs, spanEnd) - bar.startTime, 0);
    }
    return bar.durationMs;
  }

  const pxPerMs = totalSpan > 0 && trackWidth > 0 ? trackWidth / totalSpan : 1;

  // Built from all rows (not the filtered subset) so the gap/segment layout —
  // and thus the scale — stays fixed as filters toggle rows in and out.
  const rawIntervals: [number, number][] = [];
  for (const rows of rowsByKey.values()) {
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
    // No 1px floor here: point events get their visual minimum at render time
    // (MIN_BAR_PX). Flooring the *mapping* made dispPos creep by 1px per point
    // event, nudging fitScale and reflowing the whole track on every append.
    const evW = (iEnd - iStart) * pxPerMs;
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
  // Trailing segment: cover the span from the last event interval up to the
  // (quantized) span end. Without it, that trailing time is compressed to
  // zero width, which makes the track width oscillate while events stream:
  // each resource tick grows the span (shrinking pxPerMs and every segment
  // with it — track shrinks), then the next real event reveals the trailing
  // region as a brand-new gap segment at full natural width (track expands
  // again). With the mapping always covering [minTime, spanEnd], the natural
  // width is span·pxPerMs = trackWidth·span/totalSpan, which is constant, so
  // the content width can never flip the horizontal scrollbar on and off.
  if (spanEnd > prevEnd) {
    const gapMs = spanEnd - prevEnd;
    const gapNatural = gapMs * pxPerMs;
    segments.push({
      realStart: prevEnd,
      realEnd: spanEnd,
      dispStart: dispPos,
      dispEnd: dispPos + gapNatural,
      isGap: gapMs >= GAP_THRESHOLD_MS,
    });
    dispPos += gapNatural;
  }

  const fitScale =
    trackWidth > 0 && dispPos > 0 && dispPos + SCROLLBAR_GUTTER_PX < trackWidth
      ? (trackWidth - SCROLLBAR_GUTTER_PX) / dispPos
      : 1;
  for (const seg of segments) {
    seg.dispStart *= fitScale;
    seg.dispEnd *= fitScale;
  }
  // In fit mode the content fills `trackWidth - gutter` (via fitScale). Pin the
  // wrapper to that same width — not the full `trackWidth` — so it never equals
  // the scroller's client width exactly and can't fight the vertical scrollbar.
  const contentTrackWidth =
    fitScale !== 1
      ? trackWidth - SCROLLBAR_GUTTER_PX
      : Math.ceil(dispPos * fitScale + SCROLLBAR_GUTTER_PX);

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
    const zDispEnd = realToDisplay(Math.min(spanEnd, zoomWindow.realEnd));
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

  // Clamp the tick count to a finite, bounded value: if a degenerate (tiny/zero)
  // panel ever produced a NaN/Infinity effective width, `Array.from({length})`
  // would throw or try to allocate an unbounded array and hang the tab.
  const tickCount = Number.isFinite(effectiveTrackWidth)
    ? Math.min(Math.max(Math.floor(effectiveTrackWidth / 100), 0) + 1, 5000)
    : 1;
  const tickPositions = Array.from(
    { length: tickCount },
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
    // Live bars have hit the reserved span end — the growth tick is a no-op
    // until new events extend the span (or never, for a recorded trace that
    // ends with a dangling in-flight request).
    liveCapped: liveNowMs >= spanEnd,
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

  // Bars belonging to the selected merge-group render as selected (its own bar
  // highlighted, others dimmed) even though the group routes through
  // selectedGroupKeys rather than selectedId. `hasSelection` unifies both.
  const groupKeySet = selectedGroupKeys ? new Set(selectedGroupKeys) : null;
  const hasSelection = selectedId !== null || groupKeySet !== null;
  const isBarSelected = (b: TimelineBar) =>
    barKey(b) === selectedId || (groupKeySet?.has(barKey(b)) ?? false);

  // Apply a click on a rendered bar/row: a multi-event group opens the grouped
  // events table (toggling off if already selected); a single event selects it.
  const selectGroup = (bars: TimelineBar[]) => {
    if (bars.length === 0) return;
    if (bars.length > 1) {
      const keys = bars.map(barKey);
      setSelectedGroupKeys((prev) =>
        prev && sameKeySet(prev, keys) ? null : keys,
      );
      setSelectedId(null);
    } else {
      const k = barKey(bars[0]);
      setSelectedId(k === selectedId ? null : k);
    }
  };

  // The timeline rows and the event-detail view are the two tiles of a mosaic
  // split, so the whole inspector resizes through one mechanism (react-mosaic)
  // rather than a bespoke row-resize drag here.
  const rowsTile = (
    <div className="relative flex flex-col h-full">
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
          // `overflow-scroll` (not auto): keep both scrollbar tracks reserved at
          // all times so their appearance can never steal width/height and
          // reflow the grid mid-stream. The timeline-scroll CSS paints the
          // idle tracks as background, so the reserved gutters read as empty.
          className="timeline-scroll scroll-container min-h-0 flex-1 overflow-scroll text-xs cursor-default select-none"
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
                    className="flex bg-sidebar cursor-pointer"
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
                            tools: "bg-indigo-500/70",
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
                          !isResource && vl.laneBars.some(isBarSelected);
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
                              // Mirror the bar click: if the first bar merges
                              // into a group, select that group (show the table)
                              // rather than the lone event.
                              if (!vl.row.isPoint && MERGE_OVERLAPS) {
                                const groups = computeMergeGroups(
                                  vl.laneBars,
                                  toDisplay,
                                  effectiveDur,
                                );
                                if (groups[0]) {
                                  selectGroup(groups[0].bars);
                                  return;
                                }
                              }
                              selectGroup([first]);
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
                                className={`truncate font-mono ${hasSelection && isLaneSelected ? "text-foreground" : "text-muted-foreground"}`}
                                // host+path labels routinely overflow the
                                // (resizable) label column; the tooltip keeps
                                // the distinguishing tail reachable.
                                title={vl.row.label}
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
                                      const isSelected = isBarSelected(bar);
                                      return (
                                        <div
                                          key={bar.id}
                                          className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 rounded-sm ${vl.row.method === "err" ? "bg-red-400/70" : "bg-zinc-500/70"} ${!isSelected && hasSelection ? "opacity-50" : ""}`}
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
                                      const isSelected = isBarSelected(bar);
                                      const isLive = isLiveBar(bar);
                                      return (
                                        <div
                                          key={bar.id}
                                          className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 ${!isSelected && hasSelection ? "opacity-50" : ""}`}
                                          style={{
                                            left: leftPx,
                                            width: rightPx - leftPx,
                                            maxWidth: `calc(100% - ${leftPx}px)`,
                                          }}
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
                                            style={{
                                              left: "50%",
                                              transform: "translateX(-50%)",
                                              width: Math.max(
                                                rightPx - leftPx,
                                                MIN_CLICK_TARGET_BAR_PX,
                                              ),
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
                                  const groups = computeMergeGroups(
                                    visible,
                                    toDisplay,
                                    effectiveDur,
                                  );
                                  return groups.map((group) => {
                                    const first = group.bars[0];
                                    const isSelected =
                                      group.bars.some(isBarSelected);
                                    const isLive = group.bars.some((b) =>
                                      isLiveBar(b),
                                    );
                                    const w = group.rightPx - group.leftPx;
                                    return (
                                      <div
                                        key={first.id}
                                        className={`group/bar absolute top-1/2 -translate-y-1/2 h-4 ${!isSelected && hasSelection ? "opacity-50" : ""}`}
                                        style={{
                                          left: group.leftPx,
                                          width: w,
                                          maxWidth: `calc(100% - ${group.leftPx}px)`,
                                        }}
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
                                          style={{
                                            left: "50%",
                                            transform: "translateX(-50%)",
                                            width: Math.max(
                                              w,
                                              MIN_CLICK_TARGET_BAR_PX,
                                            ),
                                          }}
                                          onClick={(e) => {
                                            e.stopPropagation();
                                            if (dragHappenedRef.current) return;
                                            // Multi-event bar → grouped events
                                            // table; single event → its detail.
                                            selectGroup(group.bars);
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
  );

  const detailTile = selectedGroupBars ? (
    <div className="flex h-full flex-col overflow-hidden scroll-container detail-scroll">
      <GroupEventsTable
        bars={selectedGroupBars}
        selectedId={selectedId}
        onSelect={setSelectedId}
        onExpand={expandDetail}
      />
    </div>
  ) : selectedBar ? (
    <div className="flex h-full flex-col overflow-hidden scroll-container detail-scroll">
      <RowDetailPanel
        key={barKey(selectedBar)}
        bar={selectedBar}
        prevBar={prevAnthropicBar}
        onPrev={prevBarId !== null ? selectPrevBar : undefined}
        onNext={nextBarId !== null ? selectNextBar : undefined}
        onExpand={expandDetail}
        applyConfig={applyConfig}
        onOpenFile={onOpenFile}
      />
    </div>
  ) : null;

  // Only split off the detail tile when an event (or merge-group) is selected;
  // otherwise the timeline rows fill the panel. (The single-column layout shows
  // the detail in a dialog instead — `detailInDialog` — so it never splits here.)
  const detailVisible =
    (selectedBar !== null || selectedGroupBars !== null) && !detailInDialog;
  const mosaicValue: MosaicNode<TimelinePane> = detailVisible
    ? {
        type: "split",
        direction: "column",
        children: ["rows", "detail"],
        splitPercentages: [detailSplit, 100 - detailSplit],
      }
    : "rows";

  return (
    <div ref={containerRef} className="flex flex-col h-full">
      <div className="hive-mosaic relative min-h-0 flex-1 overflow-hidden">
        <MosaicWithoutDragDropContext<TimelinePane>
          className=""
          value={mosaicValue}
          onChange={(node) => {
            if (
              node &&
              typeof node !== "string" &&
              node.type === "split" &&
              node.splitPercentages
            )
              setDetailSplit(node.splitPercentages[0]);
          }}
          onRelease={(node) => {
            if (
              node &&
              typeof node !== "string" &&
              node.type === "split" &&
              node.splitPercentages
            )
              localStorage.setItem(
                "timeline:detailSplit",
                String(node.splitPercentages[0]),
              );
          }}
          renderTile={(paneId) =>
            paneId === "detail" && detailTile ? detailTile : rowsTile
          }
        />
      </div>

      {(selectedBar || selectedGroupBars) && (
        <Dialog
          open={detailInDialog ? true : detailExpanded}
          onOpenChange={(open) => {
            if (open) return;
            // In single-column mode the dialog *is* the selection, so closing it
            // deselects the event; otherwise it just collapses the expanded view.
            if (detailInDialog) {
              setSelectedId(null);
              setSelectedGroupKeys(null);
            } else setDetailExpanded(false);
          }}
        >
          <DialogContent className="max-w-7xl p-0 flex flex-col overflow-hidden h-[80vh] scroll-container detail-scroll">
            <DialogTitle className="sr-only">Event detail</DialogTitle>
            {selectedGroupBars ? (
              <GroupEventsTable
                bars={selectedGroupBars}
                selectedId={selectedId}
                onSelect={setSelectedId}
                expandedView
              />
            ) : selectedBar ? (
              <RowDetailPanel
                key={barKey(selectedBar)}
                bar={selectedBar}
                prevBar={prevAnthropicBar}
                onPrev={prevBarId !== null ? selectPrevBar : undefined}
                onNext={nextBarId !== null ? selectNextBar : undefined}
                applyConfig={applyConfig}
                onOpenFile={onOpenFile}
                expandedView
              />
            ) : null}
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}

// Memoized so unrelated SandboxDetail state changes (ports/isolation loading,
// browser tabs, file preview dialog, etc.) don't re-render this heavy component;
// it re-renders only when its own props change (events, filter, selection).
export const TimelineView = memo(TimelineViewInner);
