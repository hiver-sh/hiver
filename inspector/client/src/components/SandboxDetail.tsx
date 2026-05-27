import { Activity, ExternalLink, Filter, FolderTree, Loader2, Power, SlidersHorizontal, SquareTerminal, X } from "lucide-react";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { SandboxConfigDialog } from "@/components/SandboxConfigDialog";
import type { ConfigProposal } from "@/components/SandboxConfigDialog";
import { Terminal } from "@/components/Terminal";
import { FileExplorer } from "@/components/FileExplorer";
import { TimelineView, EMPTY_FILTER, KIND_OPTIONS, ACCESS_OPTIONS, isFilterActive, buildRows, applyFilter } from "@/components/TimelineView";
import type { FilterKind, FilterAccess, FilterState } from "@/components/TimelineView";
import { Separator } from "@/components/ui/separator";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { CodeViewer } from "@/components/CodeViewer";
import { cn } from "@/lib/utils";
import { langForPath } from "@/lib/fileUtils";
import type { SandboxEvent, SandboxRef } from "@/types";

interface Props {
  sandbox: SandboxRef;
  serverUrl: string;
  controllerUrl: string;
  onShutdown: () => void;
  onConnectedChange?: (connected: boolean) => void;
}

export function SandboxDetail({ sandbox, serverUrl, controllerUrl, onShutdown, onConnectedChange }: Props) {
  const [events, setEvents] = useState<SandboxEvent[]>([]);
  const [connected, setConnected] = useState(false);
  useEffect(() => { onConnectedChange?.(connected); }, [connected]); // eslint-disable-line react-hooks/exhaustive-deps
const [shutdownLoading, setShutdownLoading] = useState(false);
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
  const [showTerminal, setShowTerminal] = useState(
    () => localStorage.getItem("sandbox:showTerminal") === "true",
  );

  useEffect(() => {
    localStorage.setItem("sandbox:showTerminal", String(showTerminal));
  }, [showTerminal]);
  const [showFiles, setShowFiles] = useState(
    () => localStorage.getItem("sandbox:showFiles") === "true",
  );

  useEffect(() => {
    localStorage.setItem("sandbox:showFiles", String(showFiles));
  }, [showFiles]);
  const [filesWidth, setFilesWidth] = useState(
    () => parseInt(localStorage.getItem("sandbox:filesWidth") ?? "256", 10),
  );

  useEffect(() => {
    localStorage.setItem("sandbox:filesWidth", String(filesWidth));
  }, [filesWidth]);
  const [showTimeline, setShowTimeline] = useState(true);
  const [showConfig, setShowConfig] = useState(false);
  const [configProposal, setConfigProposal] = useState<ConfigProposal | undefined>();
  const [terminalWidth, setTerminalWidth] = useState(
    () => parseInt(localStorage.getItem("sandbox:terminalWidth") ?? "480", 10),
  );

  useEffect(() => {
    localStorage.setItem("sandbox:terminalWidth", String(terminalWidth));
  }, [terminalWidth]);
  const contentRef = useRef<HTMLDivElement>(null);
  const abortRef = useRef<AbortController | null>(null);
  const [filePreview, setFilePreview] = useState<{ path: string; content: string; lang: string } | null>(null);

  const openFile = useCallback(async (path: string) => {
    const lang = langForPath(path);
    if (!lang) return;
    const url = new URL(`${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/file`);
    url.searchParams.set("path", path);
    url.searchParams.set("controller", controllerUrl);
    try {
      const content = await fetch(url).then((r) => r.text());
      setFilePreview({ path, content, lang });
    } catch { /* ignore */ }
  }, [sandbox.id, serverUrl, controllerUrl]);

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

  const fsWriteEvents = useMemo(
    () => events.filter(e => e.type === "fs.request" && e.operation === "write"),
    [events],
  );

  const rows = useMemo(() => buildRows(events), [events]);
  const filteredRows = useMemo(() => applyFilter(rows, filter), [rows, filter]);

  const totalBars = useMemo(() => rows.reduce((sum, r) => sum + r.bars.length, 0), [rows]);
  const filteredTotalBars = useMemo(() => filteredRows.reduce((sum, r) => sum + r.bars.length, 0), [filteredRows]);

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

  function makeDragHandler(setWidth: (w: number) => void, currentWidth: number) {
    return function startDrag(e: React.MouseEvent) {
      e.preventDefault();
      const startX = e.clientX;
      const startWidth = currentWidth;

      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";

      function onMove(e: MouseEvent) {
        const delta = startX - e.clientX;
        setWidth(Math.max(240, Math.min(startWidth + delta, 1200)));
      }

      function onUp() {
        document.body.style.cursor = "";
        document.body.style.userSelect = "";
        document.removeEventListener("mousemove", onMove);
        document.removeEventListener("mouseup", onUp);
      }

      document.addEventListener("mousemove", onMove);
      document.addEventListener("mouseup", onUp);
    };
  }

  return (
    <div className="flex h-full flex-col">
      {/* Header */}
      <div className="flex h-[70px] items-start justify-between gap-4 p-4 pb-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <h2 className="truncate text-base font-semibold">{sandbox.id}</h2>
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
            variant={showTimeline ? "secondary" : "ghost"}
            onClick={() => setShowTimeline((v) => !v)}
            title="Toggle timeline"
          >
            <Activity className="h-4 w-4" />
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
            variant={showFiles ? "secondary" : "ghost"}
            onClick={() => setShowFiles((v) => !v)}
            title="Toggle file explorer"
          >
            <FolderTree className="h-4 w-4" />
          </Button>
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

      {/* Content */}
      <div ref={contentRef} className="min-h-0 flex-1 flex overflow-hidden">
        {showTimeline && (
          <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
            {/* Toolbar: event count + filter + clear */}
            <div className="flex items-center justify-between px-5 py-1.5 text-xs text-muted-foreground border-b border-border">
              <span>
                {isFilterActive(filter)
                  ? <>{filteredTotalBars} <span className="text-muted-foreground/40">/ {totalBars}</span></>
                  : totalBars
                }{" "}event{totalBars !== 1 ? "s" : ""}
              </span>
              <div className="flex items-stretch gap-3">
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
            <div className="min-h-0 flex-1 overflow-hidden">
              <TimelineView events={events} filter={filter} applyConfig={proposePolicy} onOpenFile={openFile} />
            </div>
          </div>
        )}

        {showTerminal && (
          <>
            {showTimeline && (
              <div
                className="w-[5px] shrink-0 cursor-col-resize bg-border hover:bg-white/40 transition-colors"
                onMouseDown={makeDragHandler(setTerminalWidth, terminalWidth)}
              />
            )}
            <div
              className="overflow-hidden"
              style={showTimeline ? { width: terminalWidth, flexShrink: 0 } : { flex: 1 }}
            >
              <Terminal
                sandboxId={sandbox.id}
                serverUrl={serverUrl}
                sshHost={sandbox.exposed_endpoint?.split(":")[0] ?? "127.0.0.1"}
                sshPort={parseInt(sandbox.exposed_endpoint?.split(":")[1] ?? "22")}
              />
            </div>
          </>
        )}

        {showFiles && (
          <>
            {(showTimeline || showTerminal) && (
              <div
                className="w-[5px] shrink-0 cursor-col-resize bg-border hover:bg-white/40 transition-colors"
                onMouseDown={makeDragHandler(setFilesWidth, filesWidth)}
              />
            )}
            <div
              className="overflow-hidden"
              style={(showTimeline || showTerminal) ? { width: filesWidth, flexShrink: 0 } : { flex: 1 }}
            >
              <FileExplorer
                sandboxId={sandbox.id}
                serverUrl={serverUrl}
                controllerUrl={controllerUrl}
                events={fsWriteEvents}
              />
            </div>
          </>
        )}
      </div>

      <Dialog open={filePreview !== null} onOpenChange={(open) => { if (!open) setFilePreview(null); }}>
        <DialogContent className="max-w-4xl">
          <div className="pr-6">
            <DialogTitle className="truncate font-mono text-sm font-normal text-muted-foreground">
              {filePreview?.path}
            </DialogTitle>
          </div>
          <div className="h-[55vh] overflow-hidden rounded-md border border-border">
            {filePreview && <CodeViewer content={filePreview.content} lang={filePreview.lang} />}
          </div>
        </DialogContent>
      </Dialog>

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
