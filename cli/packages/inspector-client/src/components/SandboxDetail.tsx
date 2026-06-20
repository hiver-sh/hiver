import {
  Activity,
  Filter,
  FolderTree,
  Loader2,
  LocateFixed,
  Pause,
  Play,
  Power,
  SlidersHorizontal,
  SquareTerminal,
  Trash2,
  X,
} from "lucide-react";

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useSearchParams } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { SandboxConfigDialog } from "@/components/SandboxConfigDialog";
import type { ConfigProposal } from "@/components/SandboxConfigDialog";
import { Terminal, type TerminalSink } from "@/components/Terminal";
import { PortUsageDialog } from "@/components/PortUsageDialog";
import { FileExplorer } from "@/components/FileExplorer";
import {
  TimelineView,
  EMPTY_FILTER,
  KIND_OPTIONS,
  ACCESS_OPTIONS,
  isFilterActive,
  buildRows,
  applyFilter,
} from "@/components/TimelineView";
import type {
  FilterKind,
  FilterAccess,
  FilterState,
} from "@/components/TimelineView";
import { Separator } from "@/components/ui/separator";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { CodeViewer } from "@/components/CodeViewer";
import { cn } from "@/lib/utils";
import { langForPath } from "@/lib/fileUtils";
import type { SandboxEvent, SandboxRef } from "@/types";
import { loadEvents, appendEvent, clearEvents } from "@/lib/eventStore";
import { useTransport } from "@/lib/transport";
import { useUserPreferences } from "@/lib/userPreferences";
import type { PanelId } from "@/lib/userPreferences";

const MIN_TIMELINE_WIDTH = 300;
const DRAG_HANDLE_WIDTH = 5;
// Smallest height a panel keeps when the layout stacks vertically.
const MIN_PANEL_HEIGHT = 140;

export interface SandboxDetailProps {
  sandbox: SandboxRef;
  serverUrl: string;
  onConnectedChange?: (connected: boolean) => void;
}

export function SandboxDetail({
  sandbox,
  serverUrl,
  onConnectedChange,
}: SandboxDetailProps) {
  const { transport, player, gatewayUrl } = useTransport();
  const { prefs, setPref } = useUserPreferences();
  const [events, setEvents] = useState<SandboxEvent[]>([]);
  const [connected, setConnected] = useState(false);
  useEffect(() => {
    onConnectedChange?.(connected);
  }, [connected]); // eslint-disable-line react-hooks/exhaustive-deps
  const [shutdownLoading, setShutdownLoading] = useState(false);
  // Shutting down = either our request is in flight, or the lifecycle stream
  // has reported the sandbox is no longer running.
  const isShuttingDown =
    shutdownLoading || sandbox.status === "stop" || sandbox.status === "die";
  const [ports, setPorts] = useState<number[]>([]);
  const [isolation, setIsolation] = useState<string | null>(null);
  // null = dialog closed; { port } = open for that port (port null = no exposed ports).
  const [portDialog, setPortDialog] = useState<{ port: number | null } | null>(
    null,
  );
  const [searchParams, setSearchParams] = useSearchParams();
  const [zoomWindow, setZoomWindow] = useState<{
    realStart: number;
    realEnd: number;
  } | null>(null);
  const follow = prefs.followEvents;
  const setFollow = (v: boolean) => setPref("followEvents", v);
  const [streamingPaused, setStreamingPaused] = useState(false);

  const [filter, setFilter] = useState<FilterState>(() => {
    const kind = searchParams.get("filter-kind") ?? "all";
    const access = searchParams.get("filter-access") ?? "all";
    return {
      kind: (["all", "egress", "fs", "llm"] as FilterKind[]).includes(
        kind as FilterKind,
      )
        ? (kind as FilterKind)
        : "all",
      access: (["all", "allowed", "denied"] as FilterAccess[]).includes(
        access as FilterAccess,
      )
        ? (access as FilterAccess)
        : "all",
      query: searchParams.get("filter-q") ?? "",
    };
  });

  useEffect(() => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (filter.kind !== "all") next.set("filter-kind", filter.kind);
        else next.delete("filter-kind");
        if (filter.access !== "all") next.set("filter-access", filter.access);
        else next.delete("filter-access");
        if (filter.query) next.set("filter-q", filter.query);
        else next.delete("filter-q");
        return next;
      },
      { replace: true },
    );
  }, [filter]); // eslint-disable-line react-hooks/exhaustive-deps
  const showTerminal = prefs.showTerminal;
  const setShowTerminal = (v: boolean) => setPref("showTerminal", v);
  const showFiles = prefs.showFiles;
  const setShowFiles = (v: boolean) => setPref("showFiles", v);
  const showTimeline = prefs.showTimeline;
  const setShowTimeline = (v: boolean) => setPref("showTimeline", v);
  const panelSizes = prefs.panelSizes;
  const terminalPanel = panelSizes.find((p) => p.id === "terminal")!;
  const filesPanel = panelSizes.find((p) => p.id === "files")!;
  const setPanelWidth = (id: PanelId, v: number) =>
    setPref(
      "panelSizes",
      panelSizes.map((p) => (p.id === id ? { ...p, width: v } : p)),
    );
  const setFilesWidth = (v: number) => setPanelWidth("files", v);
  const setTerminalWidth = (v: number) => setPanelWidth("terminal", v);
  // Width of each side panel: the user's resized value, or the panel default.
  const terminalWidth = terminalPanel.width ?? terminalPanel.defaultWidth;
  const filesWidth = filesPanel.width ?? filesPanel.defaultWidth;
  // Measured width of the panel row. Used to decide when the panels can no
  // longer all satisfy their min widths and the layout should stack vertically.
  const [contentWidth, setContentWidth] = useState(0);
  const requiredWidth =
    (showTimeline ? MIN_TIMELINE_WIDTH : 0) +
    (showTerminal ? terminalPanel.minWidth + DRAG_HANDLE_WIDTH : 0) +
    (showFiles ? filesPanel.minWidth + DRAG_HANDLE_WIDTH : 0);
  // Below the combined min widths, stack panels vertically instead of side by
  // side. `contentWidth === 0` means we haven't measured yet — stay horizontal.
  const vertical = contentWidth > 0 && contentWidth < requiredWidth;
  const [showConfig, setShowConfig] = useState(false);
  const [configProposal, setConfigProposal] = useState<
    ConfigProposal | undefined
  >();
  const contentRef = useRef<HTMLDivElement>(null);
  const abortRef = useRef<AbortController | null>(null);
  const reconnectRef = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );
  // Terminal channel of the shared per-sandbox stream: the currently-mounted
  // Terminal's sink (if any) and whether the upstream terminal is attached.
  const termSinkRef = useRef<TerminalSink | null>(null);
  const termConnectedRef = useRef(false);
  const resumeFromIdRef = useRef<number | undefined>(undefined);
  const [filePreview, setFilePreview] = useState<{
    path: string;
    content: string;
    lang: string;
  } | null>(null);

  // Track the panel row width so the default panel split can be expressed as a
  // percentage of the available space.
  useLayoutEffect(() => {
    const el = contentRef.current;
    if (!el) return;
    const update = () => setContentWidth(el.getBoundingClientRect().width);
    update();
    const ro = new ResizeObserver(update);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Load the sandbox's exposed ports (image EXPOSE directives) for the header.
  useEffect(() => {
    let cancelled = false;
    setPorts([]);
    const url = new URL(
      `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(sandbox.key)}/ports`,
    );
    transport
      .fetch(url)
      .then((r) => r.json() as Promise<{ ports?: number[] }>)
      .then((data) => {
        if (!cancelled) setPorts(data.ports ?? []);
      })
      .catch(() => {
        /* ignore */
      });
    return () => {
      cancelled = true;
    };
  }, [sandbox.id, sandbox.key, serverUrl, transport]);

  // Load the sandbox's isolation mechanism (container/microvm) for the header.
  // Isolation is determined at boot from the image, not configured, so it comes
  // from the sandbox's /info endpoint rather than its config.
  useEffect(() => {
    let cancelled = false;
    setIsolation(null);
    const url = new URL(
      `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(sandbox.key)}/info`,
    );
    transport
      .fetch(url)
      .then((r) => r.json() as Promise<{ isolation?: string }>)
      .then((info) => {
        if (!cancelled) setIsolation(info.isolation ?? "container");
      })
      .catch(() => {
        /* ignore */
      });
    return () => {
      cancelled = true;
    };
  }, [sandbox.id, sandbox.key, serverUrl, transport]);

  const openFile = useCallback(
    async (path: string) => {
      const lang = langForPath(path);
      if (!lang) return;
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(sandbox.key)}/file`,
      );
      url.searchParams.set("path", path);

      try {
        const content = await transport.fetch(url).then((r) => r.text());
        setFilePreview({ path, content, lang });
      } catch {
        /* ignore */
      }
    },
    [sandbox.id, sandbox.key, serverUrl, transport],
  );

  const proposePolicy = useCallback(
    async (
      updater: (cfg: Record<string, unknown>) => Record<string, unknown>,
    ) => {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(sandbox.key)}/config`,
      );

      const current = await transport
        .fetch(url)
        .then((r) => r.json() as Promise<Record<string, unknown>>);
      setConfigProposal({
        current: JSON.stringify(current, null, 2),
        proposed: JSON.stringify(updater(current), null, 2),
      });
      setShowConfig(true);
    },
    [sandbox.id, sandbox.key, serverUrl, transport],
  );

  const fsWriteEvents = useMemo(
    () =>
      events.filter(
        (e): e is Extract<SandboxEvent, { type: "fs.request" }> =>
          e.type === "fs.request" &&
          (e as Extract<SandboxEvent, { type: "fs.request" }>).operation ===
            "write",
      ),
    [events],
  );

  const rows = useMemo(() => buildRows(events), [events]);
  const filteredRows = useMemo(() => applyFilter(rows, filter), [rows, filter]);

  const totalBars = useMemo(
    () => rows.reduce((sum, r) => sum + r.bars.length, 0),
    [rows],
  );
  const filteredTotalBars = useMemo(
    () => filteredRows.reduce((sum, r) => sum + r.bars.length, 0),
    [filteredRows],
  );

  // A mounted Terminal registers here to receive output from the shared stream.
  // If the upstream terminal is already attached when this Terminal mounts (the
  // panel was opened after the stream connected), nudge it to repaint: onConnected
  // makes the Terminal re-send its size, and the server forces a SIGWINCH, so a
  // full-screen TUI redraws its current screen.
  const subscribeTerminal = useCallback((sink: TerminalSink) => {
    termSinkRef.current = sink;
    if (termConnectedRef.current) sink.onConnected();
    return () => {
      if (termSinkRef.current === sink) termSinkRef.current = null;
    };
  }, []);

  // The per-sandbox event feed AND terminal output share ONE SSE connection
  // (`/stream`), so a tab holds a single long-lived connection instead of two —
  // staying under the browser's ~6-per-origin HTTP/1.1 cap with several tabs
  // open. Frames are namespaced: `feed` (a SandboxEvent), `term` (base64 pty
  // bytes), `term:connected` / `term:close` (terminal lifecycle). On a dropped
  // connection we reconnect and resume the feed from the last id; the server
  // re-attaches the terminal and replays its scrollback.
  const startStream = useCallback(
    (lastEventId?: number) => {
      abortRef.current?.abort();
      const ac = new AbortController();
      abortRef.current = ac;
      setConnected(false);
      let resumeId = lastEventId;

      const run = async () => {
        if (ac.signal.aborted) return;
        const url = new URL(
          `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(sandbox.key)}/stream`,
        );
        if (resumeId !== undefined)
          url.searchParams.set("lastEventId", String(resumeId));

        let res: Response;
        try {
          res = await transport.fetch(url.toString(), { signal: ac.signal });
        } catch {
          if (!ac.signal.aborted && !player)
            reconnectRef.current = setTimeout(run, 2000);
          return;
        }
        if (!res.ok || !res.body) {
          if (!ac.signal.aborted && !player)
            reconnectRef.current = setTimeout(run, 2000);
          return;
        }
        setConnected(true);

        const reader = res.body.getReader();
        const dec = new TextDecoder();
        let buf = "";
        try {
          while (true) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += dec.decode(value, { stream: true });
            let sep: number;
            while ((sep = buf.indexOf("\n\n")) !== -1) {
              const block = buf.slice(0, sep);
              buf = buf.slice(sep + 2);
              let eventName = "message";
              let dataLine = "";
              for (const line of block.split("\n")) {
                if (line.startsWith("event: ")) eventName = line.slice(7);
                else if (line.startsWith("data: ")) dataLine = line.slice(6);
              }
              if (eventName === "feed") {
                try {
                  const event = JSON.parse(dataLine) as SandboxEvent;
                  // Always advance resumeId so reconnects resume at the right
                  // offset even when we skip a duplicate below.
                  resumeId = event.id;
                  // Deduplicate: the reader may flush buffered bytes after an
                  // abort, and the subsequent reconnect re-delivers those same
                  // events from the broker ring. Only add events with a strictly
                  // higher id than the last one already in state.
                  setEvents((prev) => {
                    const last = prev[prev.length - 1];
                    if (last && last.id >= event.id) return prev;
                    return [...prev, event];
                  });
                  if (!player) void appendEvent(`${sandbox.id}:${sandbox.key}`, event);
                } catch {
                  // ignore malformed frame
                }
              } else if (eventName === "term") {
                termSinkRef.current?.onData(dataLine);
              } else if (eventName === "term:connected") {
                termConnectedRef.current = true;
                termSinkRef.current?.onConnected();
              } else if (eventName === "term:close") {
                termConnectedRef.current = false;
                termSinkRef.current?.onClose();
              }
            }
          }
        } catch {
          // read error — fall through to reconnect
        }
        setConnected(false);
        if (!ac.signal.aborted && !player)
          reconnectRef.current = setTimeout(run, 2000);
      };

      ac.signal.addEventListener("abort", () => {
        if (reconnectRef.current) clearTimeout(reconnectRef.current);
        setConnected(false);
      });
      void run();
    },
    [sandbox.id, sandbox.key, serverUrl, transport, player],
  );

  useEffect(() => {
    let cancelled = false;
    setEvents([]);
    if (player) {
      // In trace mode: skip stored events, replay from the start
      startStream();
      return () => {
        cancelled = true;
        abortRef.current?.abort();
      };
    }
    loadEvents(`${sandbox.id}:${sandbox.key}`)
      .then((stored) => {
        if (cancelled) return;
        setEvents(stored);
        startStream(stored[stored.length - 1]?.id);
      })
      .catch(() => {
        if (!cancelled) startStream();
      });
    return () => {
      cancelled = true;
      abortRef.current?.abort();
    };
  }, [startStream, player]); // eslint-disable-line react-hooks/exhaustive-deps

  async function handleShutdown() {
    if (!confirm(`Shut down sandbox "${sandbox.key}"?`)) return;
    // Gray the panel and show a "shutting down…" status instead of yanking the
    // user back to the home view. We don't navigate or guess at timing here:
    // useSandboxLifecycleEvents streams the sandbox's real status (stop/die)
    // and drops it from the list once it's gone, which drives the UI below.
    setShutdownLoading(true);
    try {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(sandbox.key)}/shutdown`,
      );
      await transport.fetch(url, { method: "POST" });
      void clearEvents(`${sandbox.id}:${sandbox.key}`);
    } catch {
      // Shutdown failed — drop the overlay so the panel is usable again.
      setShutdownLoading(false);
    }
  }

  function makeDragHandler(
    setWidth: (w: number) => void,
    currentWidth: number,
    minWidth: number,
    getMaxWidth: () => number,
  ) {
    return function startDrag(e: React.MouseEvent) {
      e.preventDefault();
      const startX = e.clientX;
      const startWidth = currentWidth;
      const maxWidth = getMaxWidth();

      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";

      function onMove(e: MouseEvent) {
        const delta = startX - e.clientX;
        setWidth(Math.max(minWidth, Math.min(startWidth + delta, maxWidth)));
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
    <div
      className={cn(
        "flex h-full flex-col transition-[filter,opacity] duration-300",
        isShuttingDown && "pointer-events-none grayscale opacity-50",
      )}
      aria-busy={isShuttingDown}
    >
      {/* Header */}
      <div className="flex h-[70px] items-center justify-between gap-4 p-4 pb-3">
        <div className="flex min-w-0 items-center gap-2">
          <h2 className="truncate text-base font-semibold">{sandbox.key}</h2>
          {isolation && (
            <span
              title={`Isolation: ${isolation}`}
              className="shrink-0 rounded border border-border bg-muted/40 px-1.5 py-0.5 text-[11px] text-muted-foreground"
            >
              {isolation}
            </span>
          )}
          {isShuttingDown && (
            <span className="flex shrink-0 items-center gap-1.5 rounded border border-border bg-muted/40 px-1.5 py-0.5 text-[11px] text-muted-foreground">
              <Loader2 className="h-3 w-3 animate-spin" />
              shutting down…
            </span>
          )}
          <div className="flex shrink-0 items-center gap-1">
            {ports.length > 0 ? (
              ports.map((port) => (
                <button
                  key={port}
                  onClick={() => setPortDialog({ port })}
                  title={`Exposed port ${port} — show SDK usage`}
                  className="rounded border border-border bg-muted/40 px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                >
                  :{port}
                </button>
              ))
            ) : (
              <button
                onClick={() => setPortDialog({ port: null })}
                title="No exposed ports — show SDK usage"
                className="rounded border border-border bg-muted/40 px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              >
                :
              </button>
            )}
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            size="sm"
            variant={showTimeline ? "secondary" : "ghost"}
            onClick={() => setShowTimeline(!showTimeline)}
            title="Toggle timeline"
          >
            <Activity className="h-4 w-4" />
          </Button>
          <Button
            size="sm"
            variant={showTerminal ? "secondary" : "ghost"}
            onClick={() => setShowTerminal(!showTerminal)}
            title="Toggle terminal panel"
          >
            <SquareTerminal className="h-4 w-4" />
          </Button>
          <Button
            size="sm"
            variant={showFiles ? "secondary" : "ghost"}
            onClick={() => setShowFiles(!showFiles)}
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
            disabled={isShuttingDown}
          >
            {isShuttingDown ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <Power className="h-4 w-4" />
            )}
          </Button>
        </div>
      </div>

      <Separator />

      {/* Content */}
      <div
        ref={contentRef}
        className={cn(
          "min-h-0 flex-1 flex",
          vertical
            ? "flex-col overflow-y-auto overflow-x-hidden"
            : "overflow-x-auto overflow-y-hidden",
        )}
      >
        {showTimeline && (
          <div
            className="flex flex-1 flex-col overflow-hidden"
            style={
              vertical
                ? { minHeight: MIN_PANEL_HEIGHT }
                : { minWidth: MIN_TIMELINE_WIDTH }
            }
          >
            {/* Toolbar: event count + filter + clear */}
            <div className="relative flex items-center justify-between px-5 py-1.5 text-xs text-muted-foreground border-b border-border">
              <span>
                {isFilterActive(filter) ? (
                  <>
                    {filteredTotalBars}{" "}
                    <span className="text-muted-foreground/40">
                      / {totalBars}
                    </span>
                  </>
                ) : (
                  totalBars
                )}{" "}
                event{totalBars !== 1 ? "s" : ""}
              </span>
              {zoomWindow && (
                <button
                  className="absolute left-1/2 -translate-x-1/2 text-[11px] text-muted-foreground bg-muted/40 hover:bg-muted/70 border border-border rounded px-2 py-0.5 transition-colors"
                  onClick={() => setZoomWindow(null)}
                  title="Reset zoom (Esc)"
                >
                  × reset zoom
                </button>
              )}
              <div className="flex items-stretch gap-3">
                <Popover>
                  <PopoverTrigger asChild>
                    <button
                      className={cn(
                        "flex items-center gap-1.5 rounded-md border px-2 py-0.5 transition-colors",
                        isFilterActive(filter)
                          ? "border-blue-600/60 bg-blue-600/10 text-blue-700 dark:border-blue-500/60 dark:bg-blue-500/10 dark:text-blue-400"
                          : "border-border text-muted-foreground hover:bg-muted/40",
                      )}
                    >
                      <Filter className="h-3 w-3" />
                      {isFilterActive(filter)
                        ? [
                            filter.kind !== "all" &&
                              KIND_OPTIONS.find((o) => o.value === filter.kind)
                                ?.label,
                            filter.access !== "all" &&
                              ACCESS_OPTIONS.find(
                                (o) => o.value === filter.access,
                              )?.label,
                            filter.query || null,
                          ]
                            .filter(Boolean)
                            .join(" · ")
                        : "Filter"}
                    </button>
                  </PopoverTrigger>
                  <PopoverContent className="w-max p-2 flex flex-col gap-2">
                    <input
                      autoFocus
                      type="text"
                      placeholder="Search domain or path…"
                      value={filter.query}
                      onChange={(e) =>
                        setFilter((f) => ({ ...f, query: e.target.value }))
                      }
                      className="w-full rounded-md border border-border bg-background px-2 py-1 text-[11px] outline-none placeholder:text-muted-foreground/50 focus:border-blue-500/50"
                    />
                    <div className="flex gap-1">
                      {KIND_OPTIONS.map((opt) => (
                        <button
                          key={opt.value}
                          onClick={() =>
                            setFilter((f) => ({ ...f, kind: opt.value }))
                          }
                          className={cn(
                            "rounded-md border px-2 py-0.5 text-[11px] transition-colors",
                            filter.kind === opt.value
                              ? "border-blue-600/60 bg-blue-600/10 text-blue-700 dark:border-blue-500/60 dark:bg-blue-500/10 dark:text-blue-400"
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
                          onClick={() =>
                            setFilter((f) => ({ ...f, access: opt.value }))
                          }
                          className={cn(
                            "rounded-md border px-2 py-0.5 text-[11px] transition-colors",
                            filter.access === opt.value
                              ? "border-blue-600/60 bg-blue-600/10 text-blue-700 dark:border-blue-500/60 dark:bg-blue-500/10 dark:text-blue-400"
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
                  onClick={() => {
                    if (streamingPaused) {
                      setStreamingPaused(false);
                      startStream(resumeFromIdRef.current);
                      resumeFromIdRef.current = undefined;
                    } else {
                      resumeFromIdRef.current = events[events.length - 1]?.id;
                      setStreamingPaused(true);
                      abortRef.current?.abort();
                      setConnected(false);
                    }
                  }}
                  title={
                    streamingPaused ? "Resume streaming" : "Pause streaming"
                  }
                  className="flex items-center gap-1.5 rounded-md border px-2 py-0.5 transition-colors border-border text-muted-foreground hover:bg-muted/40"
                >
                  {streamingPaused ? (
                    <Play className="h-3 w-3" />
                  ) : (
                    <Pause className="h-3 w-3" />
                  )}
                </button>
                <button
                  onClick={() => setFollow(!follow)}
                  title="Follow latest events"
                  className={cn(
                    "flex items-center gap-1.5 rounded-md border px-2 py-0.5 transition-colors",
                    follow
                      ? "border-blue-600/60 bg-blue-600/10 text-blue-700 dark:border-blue-500/60 dark:bg-blue-500/10 dark:text-blue-400"
                      : "border-border text-muted-foreground hover:bg-muted/40",
                  )}
                >
                  <LocateFixed className="h-3 w-3" />
                </button>
                {events.length > 0 && (
                  <button
                    onClick={() => {
                      setEvents([]);
                      void clearEvents(`${sandbox.id}:${sandbox.key}`);
                    }}
                    title="Clear events"
                    className="flex items-center gap-1.5 rounded-md border px-2 py-0.5 transition-colors border-border text-muted-foreground hover:bg-muted/40"
                  >
                    <Trash2 className="h-3 w-3" />
                  </button>
                )}
              </div>
            </div>
            <div className="min-h-0 flex-1 overflow-hidden">
              <TimelineView
                events={events}
                filter={filter}
                applyConfig={proposePolicy}
                onOpenFile={openFile}
                zoomWindow={zoomWindow}
                setZoomWindow={setZoomWindow}
                follow={follow}
                onDisableFollow={() => setFollow(false)}
                paused={streamingPaused}
                detailInDialog={vertical}
              />
            </div>
          </div>
        )}

        {showTerminal && (
          <>
            {showTimeline &&
              (vertical ? (
                <div
                  className="shrink-0 bg-border"
                  style={{ height: DRAG_HANDLE_WIDTH }}
                />
              ) : (
                <div
                  className="shrink-0 cursor-col-resize bg-border hover:bg-foreground/20 transition-colors"
                  style={{ width: DRAG_HANDLE_WIDTH }}
                  onMouseDown={makeDragHandler(
                    setTerminalWidth,
                    terminalWidth,
                    terminalPanel.minWidth,
                    () => {
                      const w =
                        contentRef.current?.getBoundingClientRect().width ??
                        1200;
                      return (
                        w -
                        MIN_TIMELINE_WIDTH -
                        (showFiles ? filesWidth + DRAG_HANDLE_WIDTH : 0) -
                        DRAG_HANDLE_WIDTH
                      );
                    },
                  )}
                />
              ))}
            <div
              className="overflow-hidden"
              style={
                vertical
                  ? { flex: 1, minHeight: MIN_PANEL_HEIGHT }
                  : showTimeline
                    ? {
                        width: terminalWidth,
                        flexShrink: 0,
                        minWidth: terminalPanel.minWidth,
                      }
                    : { flex: 1, minWidth: terminalPanel.minWidth }
              }
            >
              <Terminal
                sandboxId={sandbox.id}
                sandboxKey={sandbox.key}
                serverUrl={serverUrl}
                subscribe={subscribeTerminal}
              />
            </div>
          </>
        )}

        {showFiles && (
          <>
            {(showTimeline || showTerminal) &&
              (vertical ? (
                <div
                  className="shrink-0 bg-border"
                  style={{ height: DRAG_HANDLE_WIDTH }}
                />
              ) : (
                <div
                  className="shrink-0 cursor-col-resize bg-border hover:bg-foreground/20 transition-colors"
                  style={{ width: DRAG_HANDLE_WIDTH }}
                  onMouseDown={makeDragHandler(
                    setFilesWidth,
                    filesWidth,
                    filesPanel.minWidth,
                    () => {
                      const w =
                        contentRef.current?.getBoundingClientRect().width ??
                        1200;
                      return (
                        w -
                        MIN_TIMELINE_WIDTH -
                        (showTerminal ? terminalWidth + DRAG_HANDLE_WIDTH : 0) -
                        DRAG_HANDLE_WIDTH
                      );
                    },
                  )}
                />
              ))}
            <div
              className="overflow-hidden"
              style={
                vertical
                  ? { flex: 1, minHeight: MIN_PANEL_HEIGHT }
                  : showTimeline || showTerminal
                    ? {
                        width: filesWidth,
                        flexShrink: 0,
                        minWidth: filesPanel.minWidth,
                      }
                    : { flex: 1, minWidth: filesPanel.minWidth }
              }
            >
              <FileExplorer
                sandboxId={sandbox.id}
                sandboxKey={sandbox.key}
                serverUrl={serverUrl}
                events={fsWriteEvents}
              />
            </div>
          </>
        )}
      </div>

      <Dialog
        open={filePreview !== null}
        onOpenChange={(open) => {
          if (!open) setFilePreview(null);
        }}
      >
        <DialogContent className="max-w-4xl">
          <div className="pr-6">
            <DialogTitle className="truncate font-mono text-sm font-normal text-muted-foreground">
              {filePreview?.path}
            </DialogTitle>
          </div>
          <div className="h-[55vh] overflow-hidden rounded-md border border-border">
            {filePreview && (
              <CodeViewer
                content={filePreview.content}
                lang={filePreview.lang}
              />
            )}
          </div>
        </DialogContent>
      </Dialog>

      <PortUsageDialog
        sandboxKey={sandbox.key}
        gatewayUrl={gatewayUrl}
        open={portDialog !== null}
        port={portDialog?.port ?? null}
        onOpenChange={(open) => {
          if (!open) setPortDialog(null);
        }}
      />

      <SandboxConfigDialog
        sandboxId={sandbox.id}
        sandboxKey={sandbox.key}
        serverUrl={serverUrl}
        open={showConfig}
        onOpenChange={(open) => {
          setShowConfig(open);
          if (!open) setConfigProposal(undefined);
        }}
        proposal={configProposal}
      />
    </div>
  );
}
