import { ArrowDownToLine, ExternalLink, Filter, Loader2, Power, SlidersHorizontal, SquareTerminal, X } from "lucide-react";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { EventFeed } from "@/components/EventFeed";
import { SandboxConfigDialog } from "@/components/SandboxConfigDialog";
import type { ConfigProposal } from "@/components/SandboxConfigDialog";
import { Terminal } from "@/components/Terminal";
import { TimelineView, EMPTY_FILTER, KIND_OPTIONS, ACCESS_OPTIONS, isFilterActive, buildRows, applyFilter, filterEvents } from "@/components/TimelineView";
import type { FilterKind, FilterAccess, FilterState } from "@/components/TimelineView";
import { Separator } from "@/components/ui/separator";
import { SegmentedControl } from "@/components/SegmentedControl";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";
import type { SandboxEvent, SandboxRef } from "@/types";

interface Props {
  sandbox: SandboxRef;
  serverUrl: string;
  controllerUrl: string;
  onShutdown: () => void;
}

type View = "logs" | "timeline";

export function SandboxDetail({ sandbox, serverUrl, controllerUrl, onShutdown }: Props) {
  const [events, setEvents] = useState<SandboxEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const [autoScroll, setAutoScroll] = useState(true);
  const [shutdownLoading, setShutdownLoading] = useState(false);
  const [view, setView] = useState<View>("timeline");
  const [searchParams, setSearchParams] = useSearchParams();
  const [filter, setFilter] = useState<FilterState>(() => {
    const kind = searchParams.get("filter-kind") ?? "all";
    const access = searchParams.get("filter-access") ?? "all";
    return {
      kind: (["all", "egress", "fs", "llm"] as FilterKind[]).includes(kind as FilterKind) ? kind as FilterKind : "all",
      access: (["all", "allowed", "denied"] as FilterAccess[]).includes(access as FilterAccess) ? access as FilterAccess : "all",
      query: searchParams.get("filter-q") ?? "",
    };
  });

  useEffect(() => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (filter.kind !== "all") next.set("filter-kind", filter.kind); else next.delete("filter-kind");
      if (filter.access !== "all") next.set("filter-access", filter.access); else next.delete("filter-access");
      if (filter.query) next.set("filter-q", filter.query); else next.delete("filter-q");
      return next;
    }, { replace: true });
  }, [filter]); // eslint-disable-line react-hooks/exhaustive-deps
  const [showTerminal, setShowTerminal] = useState(false);
  const [showConfig, setShowConfig] = useState(false);
  const [configProposal, setConfigProposal] = useState<ConfigProposal | undefined>();
  const [terminalWidth, setTerminalWidth] = useState(480);
  const contentRef = useRef<HTMLDivElement>(null);
  const abortRef = useRef<AbortController | null>(null);

  const proposePolicy = useCallback(async (updater: (cfg: Record<string, unknown>) => Record<string, unknown>) => {
    const url = new URL(`${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/config`);
    url.searchParams.set("controller", controllerUrl);
    const current = await fetch(url).then((r) => r.json() as Promise<Record<string, unknown>>);
    setConfigProposal({
      current: JSON.stringify(current, null, 2),
      proposed: JSON.stringify(updater(current), null, 2),
    });
    setShowConfig(true);
  }, [sandbox.id, serverUrl, controllerUrl]);

  const rows = useMemo(() => buildRows(events), [events]);
  const filteredRows = useMemo(() => applyFilter(rows, filter), [rows, filter]);
  const filteredEvents = useMemo(() => filterEvents(events, filter), [events, filter]);

  const startStream = useCallback(() => {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setEvents([]);
    setConnected(false);

    const url = new URL(`${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/events`);
    url.searchParams.set("controller", controllerUrl);

    const es = new EventSource(url.toString());
    es.onopen = () => setConnected(true);
    es.onmessage = (e: MessageEvent<string>) => {
      try {
        const event = JSON.parse(e.data) as SandboxEvent;
        setEvents((prev) => [...prev, event]);
      } catch {
        // ignore malformed frames
      }
    };
    es.onerror = () => {
      setConnected(false);
      es.close();
    };

    ac.signal.addEventListener("abort", () => {
      es.close();
      setConnected(false);
    });
  }, [sandbox.id, serverUrl, controllerUrl]);

  useEffect(() => {
    startStream();
    return () => abortRef.current?.abort();
  }, [startStream]);

  async function handleShutdown() {
    if (!confirm(`Shut down sandbox "${sandbox.id}"?`)) return;
    setShutdownLoading(true);
    try {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/shutdown`,
      );
      url.searchParams.set("controller", controllerUrl);
      await fetch(url, { method: "POST" });
      onShutdown();
    } finally {
      setShutdownLoading(false);
    }
  }

  function startDrag(e: React.MouseEvent) {
    e.preventDefault();
    const startX = e.clientX;
    const startWidth = terminalWidth;

    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";

    function onMove(e: MouseEvent) {
      const delta = startX - e.clientX;
      setTerminalWidth(Math.max(240, Math.min(startWidth + delta, 1200)));
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

  return (
    <div className="flex h-full flex-col">
      {/* Header */}
      <div className="flex h-[70px] items-start justify-between gap-4 p-4 pb-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <h2 className="truncate text-base font-semibold">{sandbox.id}</h2>
            <Badge variant={connected ? "green" : "zinc"} className="shrink-0">
              {connected ? "live" : "disconnected"}
            </Badge>
          </div>
          <a
            href={sandbox.endpoint}
            target="_blank"
            rel="noreferrer"
            className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors mt-0.5"
          >
            <ExternalLink className="h-3 w-3" />
            {sandbox.endpoint}
          </a>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            size="sm"
            variant={showConfig ? "secondary" : "ghost"}
            onClick={() => setShowConfig(true)}
            title="Sandbox config"
          >
            <SlidersHorizontal className="h-4 w-4" />
          </Button>
          <Button
            size="sm"
            variant={showTerminal ? "secondary" : "ghost"}
            onClick={() => {
              if (!showTerminal && contentRef.current) {
                setTerminalWidth(Math.floor(contentRef.current.getBoundingClientRect().width / 2));
              }
              setShowTerminal((v) => !v);
            }}
            title="Toggle terminal panel"
          >
            <SquareTerminal className="h-4 w-4" />
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={handleShutdown}
            disabled={shutdownLoading}
          >
            {shutdownLoading ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <Power className="h-4 w-4" />
            )}
          </Button>
        </div>
      </div>

      <Separator />

      {/* Toolbar: event count + view toggle + filter + clear */}
      <div className="flex items-center justify-between px-5 py-1.5 text-xs text-muted-foreground">
        <span>
          {isFilterActive(filter)
            ? <>{view === "timeline" ? filteredRows.length : filteredEvents.length} <span className="text-muted-foreground/40">/ {rows.length}</span></>
            : rows.length
          }{" "}event{rows.length !== 1 ? "s" : ""}
        </span>
        <div className="flex items-stretch gap-3">
          <SegmentedControl
            options={[
              { value: "timeline", label: "Timeline" },
              { value: "logs", label: "Logs" },
            ]}
            value={view}
            onChange={setView}
          />
          <Popover>
              <PopoverTrigger asChild>
                <button className={cn(
                  "flex items-center gap-1.5 rounded-md border px-2 py-0.5 transition-colors",
                  isFilterActive(filter)
                    ? "border-blue-500/60 bg-blue-500/10 text-blue-400"
                    : "border-border text-muted-foreground hover:bg-muted/40",
                )}>
                  <Filter className="h-3 w-3" />
                  {isFilterActive(filter)
                    ? [
                        filter.kind !== "all" && KIND_OPTIONS.find(o => o.value === filter.kind)?.label,
                        filter.access !== "all" && ACCESS_OPTIONS.find(o => o.value === filter.access)?.label,
                        filter.query || null,
                      ].filter(Boolean).join(" · ")
                    : "Filter"}
                </button>
              </PopoverTrigger>
              <PopoverContent className="w-max p-2 flex flex-col gap-2">
                <input
                  autoFocus
                  type="text"
                  placeholder="Search domain or path…"
                  value={filter.query}
                  onChange={(e) => setFilter((f) => ({ ...f, query: e.target.value }))}
                  className="w-full rounded-md border border-border bg-background px-2 py-1 text-[11px] outline-none placeholder:text-muted-foreground/50 focus:border-blue-500/50"
                />
                <div className="flex gap-1">
                  {KIND_OPTIONS.map((opt) => (
                    <button
                      key={opt.value}
                      onClick={() => setFilter((f) => ({ ...f, kind: opt.value }))}
                      className={cn(
                        "rounded-md border px-2 py-0.5 text-[11px] transition-colors",
                        filter.kind === opt.value
                          ? "border-blue-500/60 bg-blue-500/10 text-blue-400"
                          : "border-border text-muted-foreground hover:bg-muted/40",
                      )}
                    >
                      {opt.label}
                    </button>
                  ))}
                </div>
                <div className="h-px bg-border" />
                <div className="flex gap-1">
                  {ACCESS_OPTIONS.map((opt) => (
                    <button
                      key={opt.value}
                      onClick={() => setFilter((f) => ({ ...f, access: opt.value }))}
                      className={cn(
                        "rounded-md border px-2 py-0.5 text-[11px] transition-colors",
                        filter.access === opt.value
                          ? "border-blue-500/60 bg-blue-500/10 text-blue-400"
                          : "border-border text-muted-foreground hover:bg-muted/40",
                      )}
                    >
                      {opt.label}
                    </button>
                  ))}
                </div>
                {isFilterActive(filter) && (
                  <button
                    onClick={() => setFilter(EMPTY_FILTER)}
                    className="flex items-center gap-1 text-[11px] text-muted-foreground/60 hover:text-muted-foreground transition-colors"
                  >
                    <X className="h-3 w-3" /> Clear filter
                  </button>
                )}
              </PopoverContent>
            </Popover>
          <button
            onClick={() => setAutoScroll((v) => !v)}
            title={autoScroll ? "Auto-scroll on" : "Auto-scroll off"}
            className={cn(
              "flex items-center rounded-md border px-2 py-0.5 transition-colors",
              autoScroll
                ? "border-blue-500/60 bg-blue-500/10 text-blue-400"
                : "border-border text-muted-foreground hover:bg-muted/40",
            )}
          >
            <ArrowDownToLine className="h-3 w-3" />
          </button>
          {events.length > 0 && (
            <button
              onClick={() => setEvents([])}
              className="hover:text-foreground transition-colors"
            >
              Clear
            </button>
          )}
        </div>
      </div>

      <Separator />

      {/* Content */}
      <div ref={contentRef} className="min-h-0 flex-1 flex overflow-hidden">
        <div className="min-w-0 flex-1 overflow-hidden">
          {view === "logs"
            ? <EventFeed events={events} autoScroll={autoScroll} filter={filter} />
            : <TimelineView events={events} filter={filter} autoScroll={autoScroll} applyConfig={proposePolicy} />}
        </div>

        {showTerminal && (
          <>
            <div
              className="w-[5px] shrink-0 cursor-col-resize bg-border hover:bg-white/40 transition-colors"
              onMouseDown={startDrag}
            />
            <div className="shrink-0 overflow-hidden" style={{ width: terminalWidth }}>
              <Terminal
                sandboxId={sandbox.id}
                serverUrl={serverUrl}
                sshHost={sandbox.exposed_endpoint?.split(":")[0] ?? "127.0.0.1"}
                sshPort={parseInt(sandbox.exposed_endpoint?.split(":")[1] ?? "22")}
              />
            </div>
          </>
        )}
      </div>

      <SandboxConfigDialog
        sandboxId={sandbox.id}
        serverUrl={serverUrl}
        controllerUrl={controllerUrl}
        open={showConfig}
        onOpenChange={(open) => { setShowConfig(open); if (!open) setConfigProposal(undefined); }}
        proposal={configProposal}
      />
    </div>
  );
}
