import {
  Activity,
  Camera,
  Check,
  Filter,
  FolderTree,
  Globe,
  GripVertical,
  Loader2,
  LocateFixed,
  Menu,
  Pause,
  Play,
  Plus,
  Power,
  RefreshCw,
  SlidersHorizontal,
  SquareTerminal,
  Trash2,
  X,
} from "lucide-react";

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Mosaic, MosaicWindow } from "react-mosaic-component";
import "react-mosaic-component/react-mosaic-component.css";
import { useSearchParams } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { SandboxConfigDialog } from "@/components/SandboxConfigDialog";
import type { ConfigProposal } from "@/components/SandboxConfigDialog";
import { Terminal, type TerminalSink } from "@/components/Terminal";
import {
  BrowserView,
  tabLabel,
  type BrowserSink,
  type BrowserTab,
} from "@/components/BrowserView";
import { PortUsageDialog } from "@/components/PortUsageDialog";
import { SnapshotDialog } from "@/components/SnapshotDialog";
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
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { CodeViewer } from "@/components/CodeViewer";
import { cn } from "@/lib/utils";
import { langForPath } from "@/lib/fileUtils";
import type { SandboxEvent, SandboxRef, SandboxTarget } from "@/types";
import { useTransport } from "@/lib/transport";
import { useUserPreferences, ALL_PANELS } from "@/lib/userPreferences";
import type { PanelId } from "@/lib/userPreferences";
import { usePanelLayout } from "@/lib/usePanelLayout";
import { useScrollbarVisibility } from "@/lib/useScrollbarVisibility";

// Header label for each panel.
const PANEL_TITLE: Record<PanelId, string> = {
  timeline: "Timeline",
  terminal: "Terminal",
  browser: "Browser",
  files: "Files",
};

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
  // Toggle `.scrollbar-visible` on hovered scroll-containers (timeline rows,
  // event-detail panel, files). Called here — not just in the standalone App —
  // so the hover-reveal also works when SandboxDetail is consumed via the
  // exported library (e.g. the embed), where App never mounts.
  useScrollbarVisibility();
  const { transport, player, gatewayUrl } = useTransport();
  const { prefs, setPref, showHeader } = useUserPreferences();
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
  // Whether this sandbox exposes a CDP endpoint (detected server-side). The
  // browser panel and its toggle only exist when one is available.
  const [browserAvailable, setBrowserAvailable] = useState(false);
  const showBrowser = prefs.showBrowser && browserAvailable;
  const setShowBrowser = (v: boolean) => setPref("showBrowser", v);
  // The browser tab strip is hoisted into the panel header, so tab state lives
  // here (fed from the shared stream) rather than inside BrowserView.
  const [browserTabs, setBrowserTabs] = useState<BrowserTab[]>([]);

  // Which panels are visible, in canonical order (used when adding new tiles).
  const panelVisible: Record<PanelId, boolean> = {
    timeline: showTimeline,
    terminal: showTerminal,
    browser: showBrowser,
    files: showFiles,
  };
  const visiblePanels = ALL_PANELS.filter((id) => panelVisible[id]);

  // The tiling tree Mosaic renders, plus its persist-on-release handler.
  const { layout, setLayout, persistLayout } = usePanelLayout(visiblePanels);
  const [showConfig, setShowConfig] = useState(false);
  const [showSnapshot, setShowSnapshot] = useState(false);
  const [configProposal, setConfigProposal] = useState<
    ConfigProposal | undefined
  >();
  const abortRef = useRef<AbortController | null>(null);
  const reconnectRef = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );
  // Per-sandbox-key dedup: tracks "${sandbox_key}:${id}" seen in this stream
  // session so reconnect replays don't add duplicate events.
  const seenEventKeysRef = useRef<Set<string>>(new Set());
  // Terminal channel of the shared per-sandbox stream: the currently-mounted
  // Terminal's sink (if any) and whether the upstream terminal is attached.
  const termSinkRef = useRef<TerminalSink | null>(null);
  const termConnectedRef = useRef(false);
  // Browser channel of the shared per-sandbox stream: the currently-mounted
  // BrowserView's sink (if any) and whether the upstream screencast is attached.
  const browserSinkRef = useRef<BrowserSink | null>(null);
  const browserConnectedRef = useRef(false);
  // Which sandbox actually owns the attached browser — the primary, or a nested
  // one it spawned. Carried on `browser:connected` and used to route input.
  const browserTargetRef = useRef<{ id: string; key: string } | null>(null);
  // Last browser frame + chrome state (tabs, url, nav), replayed to a BrowserView
  // that mounts after the stream already connected. The browser panel only
  // mounts once `browser:connected` flips availability on, so the `browser:tabs`
  // /url/navstate frames that arrive alongside it land before the sink exists —
  // buffering them here lets the freshly-mounted panel restore full state
  // instead of showing an empty tab strip after a refresh.
  const lastBrowserFrameRef = useRef<{
    data: string;
    width: number;
    height: number;
  } | null>(null);
  const lastBrowserTabsRef = useRef<BrowserTab[] | null>(null);
  const lastBrowserUrlRef = useRef<string | null>(null);
  const lastBrowserNavRef = useRef<{
    canGoBack: boolean;
    canGoForward: boolean;
  } | null>(null);
  const [filePreview, setFilePreview] = useState<{
    path: string;
    content: string;
    lang: string;
  } | null>(null);
  const filesRefreshRef = useRef<(() => void) | null>(null);

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

  // Browser availability is discovered from the shared stream, not a separate
  // probe: the server attaches the browser (from the primary sandbox OR a nested
  // one it spawned) and emits `browser:connected`, which flips this on. Reset it
  // when the sandbox changes so a stale panel doesn't linger.
  useEffect(() => {
    setBrowserAvailable(false);
    browserConnectedRef.current = false;
    browserTargetRef.current = null;
    lastBrowserFrameRef.current = null;
    lastBrowserTabsRef.current = null;
    lastBrowserUrlRef.current = null;
    lastBrowserNavRef.current = null;
    setBrowserTabs([]);
  }, [sandbox.id, sandbox.key]);

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

  // Events are persisted server-side (SQLite) now, so "clear" and shutdown
  // wipe them through the server rather than a local store.
  const clearStoredEvents = useCallback(() => {
    const url = new URL(
      `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(sandbox.key)}/events`,
    );
    void transport.fetch(url, { method: "DELETE" }).catch(() => {
      /* best-effort */
    });
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

  // Browser tab actions posted from the header strip. Route to the sandbox that
  // owns the attached browser (primary or a nested one), same as BrowserView.
  const postBrowserControl = useCallback(
    (body: unknown) => {
      const t = browserTargetRef.current ?? { id: sandbox.id, key: sandbox.key };
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(t.id)}/${encodeURIComponent(t.key)}/browser/control`,
      );
      void transport
        .fetch(url.toString(), {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        })
        .catch(() => {});
    },
    [sandbox.id, sandbox.key, serverUrl, transport],
  );

  const proposePolicy = useCallback(
    async (
      updater: (cfg: Record<string, unknown>) => Record<string, unknown>,
      target?: SandboxTarget,
    ) => {
      // The event may belong to a linked sandbox; route the policy edit to it.
      const id = target?.id ?? sandbox.id;
      const key = target?.key ?? sandbox.key;
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(id)}/${encodeURIComponent(key)}/config`,
      );

      const current = await transport
        .fetch(url)
        .then((r) => r.json() as Promise<Record<string, unknown>>);
      setConfigProposal({
        current: JSON.stringify(current, null, 2),
        proposed: JSON.stringify(updater(current), null, 2),
        target: { id, key },
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

  // A mounted BrowserView registers here to receive screencast frames from the
  // shared stream. If the upstream is already attached (the panel opened after
  // the stream connected), replay the connected signal — with the owning sandbox
  // so input routes correctly — and the last frame so it paints immediately.
  const subscribeBrowser = useCallback(
    (sink: BrowserSink) => {
      browserSinkRef.current = sink;
      if (browserConnectedRef.current) {
        sink.onConnected(
          browserTargetRef.current ?? { id: sandbox.id, key: sandbox.key },
        );
        // Restore the chrome state the panel missed by mounting late, so a
        // refresh shows the real tabs / address / nav instead of a bare strip.
        if (lastBrowserTabsRef.current)
          sink.onTabs(lastBrowserTabsRef.current);
        if (lastBrowserUrlRef.current !== null)
          sink.onUrl(lastBrowserUrlRef.current);
        if (lastBrowserNavRef.current)
          sink.onNavState(lastBrowserNavRef.current);
        if (lastBrowserFrameRef.current)
          sink.onFrame(lastBrowserFrameRef.current);
      }
      return () => {
        if (browserSinkRef.current === sink) browserSinkRef.current = null;
      };
    },
    [sandbox.id, sandbox.key],
  );

  // The per-sandbox event feed AND terminal output share ONE SSE connection
  // (`/stream`), so a tab holds a single long-lived connection instead of two —
  // staying under the browser's ~6-per-origin HTTP/1.1 cap with several tabs
  // open. Frames are namespaced: `feed` (a SandboxEvent), `term` (base64 pty
  // bytes), `term:connected` / `term:close` (terminal lifecycle). On a dropped
  // connection we reconnect and resume the feed from the last id; the server
  // re-attaches the terminal and replays its scrollback.
  const startStream = useCallback(
    () => {
      abortRef.current?.abort();
      const ac = new AbortController();
      abortRef.current = ac;
      setConnected(false);

      const run = async () => {
        if (ac.signal.aborted) return;
        // No lastEventId: the server persists events and replays the full
        // history on every connect. We dedupe replays via seenEventKeysRef.
        const url = new URL(
          `${serverUrl}/api/sandboxes/${encodeURIComponent(sandbox.id)}/${encodeURIComponent(sandbox.key)}/stream`,
        );

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
                  // Deduplicate per sandbox_key: the server replays the full
                  // stored history on every (re)connect, and linked-sandbox
                  // events restart from id=1 on each relay, so we can't use a
                  // single global monotonic id check.
                  const eventKey = `${event.sandbox_key ?? ""}:${event.id}`;
                  if (seenEventKeysRef.current.has(eventKey)) continue;
                  seenEventKeysRef.current.add(eventKey);
                  setEvents((prev) => [...prev, event]);
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
              } else if (eventName === "browser") {
                try {
                  const frame = JSON.parse(dataLine) as {
                    data: string;
                    width: number;
                    height: number;
                  };
                  lastBrowserFrameRef.current = frame;
                  browserSinkRef.current?.onFrame(frame);
                } catch {
                  // ignore malformed frame
                }
              } else if (eventName === "browser:connected") {
                browserConnectedRef.current = true;
                // The server tags browser events with the sandbox that owns the
                // browser (primary or a nested one), so input is routed there.
                let target = { id: sandbox.id, key: sandbox.key };
                try {
                  const d = JSON.parse(dataLine) as {
                    sandboxId?: string;
                    sandboxKey?: string;
                  };
                  if (d.sandboxId && d.sandboxKey)
                    target = { id: d.sandboxId, key: d.sandboxKey };
                } catch {
                  // fall back to the primary sandbox
                }
                browserTargetRef.current = target;
                setBrowserAvailable(true);
                browserSinkRef.current?.onConnected(target);
              } else if (eventName === "browser:url") {
                try {
                  const { url } = JSON.parse(dataLine) as { url: string };
                  lastBrowserUrlRef.current = url;
                  browserSinkRef.current?.onUrl(url);
                } catch {
                  // ignore malformed frame
                }
              } else if (eventName === "browser:navstate") {
                try {
                  const nav = JSON.parse(dataLine) as {
                    canGoBack: boolean;
                    canGoForward: boolean;
                  };
                  lastBrowserNavRef.current = nav;
                  browserSinkRef.current?.onNavState(nav);
                } catch {
                  // ignore malformed frame
                }
              } else if (eventName === "browser:tabs") {
                try {
                  const { tabs } = JSON.parse(dataLine) as {
                    tabs: BrowserTab[];
                  };
                  lastBrowserTabsRef.current = tabs;
                  setBrowserTabs(tabs);
                  browserSinkRef.current?.onTabs(tabs);
                } catch {
                  // ignore malformed frame
                }
              } else if (
                eventName === "browser:close" ||
                eventName === "browser:error"
              ) {
                browserConnectedRef.current = false;
                browserSinkRef.current?.onClose();
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
    setEvents([]);
    seenEventKeysRef.current = new Set();
    // The server replays persisted history at the head of the stream (or, in
    // trace mode, the recorded feed plays from the start), so we just connect.
    startStream();
    return () => {
      abortRef.current?.abort();
    };
  }, [startStream, player]);

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
      clearStoredEvents();
    } catch {
      // Shutdown failed — drop the overlay so the panel is usable again.
      setShutdownLoading(false);
    }
  }

  // The timeline panel's header controls (event count + filter + pause + follow
  // + clear), hoisted into the mosaic panel header. `onMouseDown` stops the
  // mosaic from starting a drag when a control is pressed, and `normal-case`
  // undoes the header's uppercasing for the filter label.
  const timelineHeaderControls = (
    <div
      className="timeline-toolbar ml-auto flex cursor-default items-center gap-2 normal-case"
      onMouseDown={(e) => e.stopPropagation()}
    >
      <span className="text-[10px] text-muted-foreground/70">
        {isFilterActive(filter) ? (
          <>
            {filteredTotalBars}
            <span className="text-muted-foreground/40"> / {totalBars}</span>
          </>
        ) : (
          totalBars
        )}{" "}
        event{totalBars !== 1 ? "s" : ""}
      </span>
      {zoomWindow && (
        <button
          className="text-[10px] text-muted-foreground bg-muted/40 hover:bg-muted/70 border border-border rounded px-2 py-0.5 transition-colors"
          onClick={() => setZoomWindow(null)}
          title="Reset zoom (Esc)"
        >
          × reset zoom
        </button>
      )}
      <Popover>
        <PopoverTrigger asChild>
          <button
            className={cn(
              "flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-[10px] transition-colors",
              isFilterActive(filter)
                ? "border-blue-600/60 bg-blue-600/10 text-blue-700 dark:border-blue-500/60 dark:bg-blue-500/10 dark:text-blue-400"
                : "border-border text-muted-foreground hover:bg-muted/40",
            )}
          >
            <Filter className="h-3 w-3" />
            {isFilterActive(filter)
              ? [
                  filter.kind !== "all" &&
                    KIND_OPTIONS.find((o) => o.value === filter.kind)?.label,
                  filter.access !== "all" &&
                    ACCESS_OPTIONS.find((o) => o.value === filter.access)
                      ?.label,
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
                onClick={() => setFilter((f) => ({ ...f, kind: opt.value }))}
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
                onClick={() => setFilter((f) => ({ ...f, access: opt.value }))}
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
            startStream();
          } else {
            setStreamingPaused(true);
            abortRef.current?.abort();
            setConnected(false);
          }
        }}
        title={streamingPaused ? "Resume streaming" : "Pause streaming"}
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
            seenEventKeysRef.current = new Set();
            clearStoredEvents();
          }}
          title="Clear events"
          className="flex items-center gap-1.5 rounded-md border px-2 py-0.5 transition-colors border-border text-muted-foreground hover:bg-muted/40"
        >
          <Trash2 className="h-3 w-3" />
        </button>
      )}
    </div>
  );

  // The browser panel's tab strip, hoisted into the mosaic panel header. Same
  // `onMouseDown`/`normal-case` treatment as the timeline controls.
  const browserHeaderControls = (
    <div
      className="ml-auto flex cursor-default items-center gap-2 normal-case opacity-0 transition-opacity group-hover/panel:opacity-100"
      onMouseDown={(e) => e.stopPropagation()}
    >
      {browserTabs.map((tab) => (
        <div
          key={tab.targetId}
          onClick={() => {
            if (!tab.active)
              postBrowserControl({
                action: "activateTab",
                targetId: tab.targetId,
              });
          }}
          title={tab.url || tabLabel(tab)}
          className={cn(
            "group flex max-w-[160px] min-w-0 shrink-0 cursor-pointer items-center gap-1.5 rounded-md border px-2 py-0.5 text-[10px] transition-colors",
            tab.active
              ? "border-blue-600/60 bg-blue-600/10 text-blue-700 dark:border-blue-500/60 dark:bg-blue-500/10 dark:text-blue-400"
              : "border-border text-muted-foreground hover:bg-muted/40",
          )}
        >
          <span className="truncate">{tabLabel(tab)}</span>
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              postBrowserControl({
                action: "closeTab",
                targetId: tab.targetId,
              });
            }}
            title="Close tab"
            className={cn(
              // -my-0.5 offsets the p-0.5 hit area so the close button doesn't
              // grow the pill past the tab's text line height.
              "-my-0.5 shrink-0 rounded p-0.5 transition hover:bg-foreground/10 hover:text-foreground",
              tab.active ? "opacity-100" : "opacity-0 group-hover:opacity-100",
            )}
          >
            <X className="h-3 w-3" />
          </button>
        </div>
      ))}
      <button
        type="button"
        onClick={() => postBrowserControl({ action: "newPage" })}
        title="Open a new page"
        className="flex shrink-0 items-center gap-1.5 rounded-md border border-border px-2 py-0.5 text-muted-foreground transition-colors hover:bg-muted/40"
      >
        <Plus className="h-3 w-3" />
      </button>
    </div>
  );

  // The inner content of each panel (the SortablePanel wrapper supplies the
  // drag-handle header and sizing).
  const timelineBody = (
    <div className="flex h-full flex-col">
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
          detailInDialog={false}
        />
      </div>
    </div>
  );

  const panelBody: Record<PanelId, React.ReactNode> = {
    timeline: timelineBody,
    terminal: (
      <Terminal
        sandboxId={sandbox.id}
        sandboxKey={sandbox.key}
        serverUrl={serverUrl}
        subscribe={subscribeTerminal}
      />
    ),
    browser: (
      <BrowserView
        sandboxId={sandbox.id}
        sandboxKey={sandbox.key}
        serverUrl={serverUrl}
        subscribe={subscribeBrowser}
      />
    ),
    files: (
      <FileExplorer
        sandboxId={sandbox.id}
        sandboxKey={sandbox.key}
        serverUrl={serverUrl}
        events={fsWriteEvents}
        refreshRef={filesRefreshRef}
      />
    ),
  };

  return (
    <div
      className={cn(
        "flex h-full flex-col transition-[filter,opacity] duration-300",
        isShuttingDown && "pointer-events-none grayscale opacity-50",
      )}
      aria-busy={isShuttingDown}
    >
      {/* Header */}
      {showHeader && <div className="flex h-12 items-center justify-between gap-4 px-4 border-b border-border">
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
          <Popover>
            <PopoverTrigger asChild>
              <Button size="sm" variant="ghost" title="View">
                <Menu className="h-4 w-4" />
              </Button>
            </PopoverTrigger>
            <PopoverContent align="end" className="w-48 p-1">
              {[
                {
                  label: "Timeline",
                  icon: Activity,
                  active: showTimeline,
                  onToggle: () => setShowTimeline(!showTimeline),
                },
                {
                  label: "Terminal",
                  icon: SquareTerminal,
                  active: showTerminal,
                  onToggle: () => setShowTerminal(!showTerminal),
                },
                ...(browserAvailable
                  ? [
                      {
                        label: "Browser",
                        icon: Globe,
                        active: showBrowser,
                        onToggle: () => setShowBrowser(!prefs.showBrowser),
                      },
                    ]
                  : []),
                {
                  label: "Files",
                  icon: FolderTree,
                  active: showFiles,
                  onToggle: () => setShowFiles(!showFiles),
                },
              ].map(({ label, icon: Icon, active, onToggle }) => (
                <button
                  key={label}
                  onClick={onToggle}
                  className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-muted/60 transition-colors"
                >
                  <Check
                    className={cn(
                      "h-3.5 w-3.5 shrink-0",
                      active ? "opacity-100" : "opacity-0",
                    )}
                  />
                  <Icon className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                  {label}
                </button>
              ))}
              <div className="my-1 h-px bg-border" />
              <button
                onClick={() => setShowSnapshot(true)}
                className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-muted/60 transition-colors"
              >
                <span className="w-3.5 shrink-0" />
                <Camera className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                Capture snapshot
              </button>
              <button
                onClick={() => setShowConfig(true)}
                className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-muted/60 transition-colors"
              >
                <span className="w-3.5 shrink-0" />
                <SlidersHorizontal className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                Sandbox config
              </button>
            </PopoverContent>
          </Popover>
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
      </div>}

      {/* Content: a tiling mosaic — drag a panel's header onto another panel's
          edge to split (side-by-side) or stack (in a column); drag the splitters
          to resize. Layout persists to prefs. */}
      <div className="hive-mosaic min-h-0 flex-1 overflow-hidden">
        <Mosaic<PanelId>
          value={layout}
          onChange={setLayout}
          onRelease={persistLayout}
          className=""
          renderTile={(id, path) => (
            <MosaicWindow<PanelId>
              path={path}
              title={PANEL_TITLE[id]}
              className="group/panel"
              toolbarControls={<span />}
              renderToolbar={() => (
                // Slim, quiet header — a bottom border, a muted label, and an
                // always-visible drag grip as the reorder affordance. (The
                // always-present mosaic drag preview behind the window is hidden
                // via CSS, so a translucent header is safe here.)
                <div className="flex h-7 w-full cursor-grab items-center gap-1 px-2 text-[10px] font-medium tracking-wide text-muted-foreground/70 uppercase active:cursor-grabbing">
                  <GripVertical className="h-3 w-3 shrink-0 text-muted-foreground/50" />
                  <span className="truncate">{PANEL_TITLE[id]}</span>
                  {id === "timeline" && timelineHeaderControls}
                  {id === "browser" && browserHeaderControls}
                  {id === "files" && (
                    <button
                      onClick={(e) => { e.stopPropagation(); filesRefreshRef.current?.(); }}
                      className="ml-auto flex cursor-pointer items-center gap-1.5 rounded-md border border-border px-2 py-0.5 text-muted-foreground opacity-0 transition-[color,background-color,opacity] hover:bg-muted/40 group-hover/panel:opacity-100"
                      title="Refresh files"
                    >
                      <RefreshCw className="h-3 w-3" />
                    </button>
                  )}
                </div>
              )}
            >
              <div className="h-full w-full overflow-hidden">
                {panelBody[id]}
              </div>
            </MosaicWindow>
          )}
          zeroStateView={
            <div className="flex h-full items-center justify-center px-4 text-center text-xs text-muted-foreground">
              No panels shown — enable one from the toolbar above.
            </div>
          }
        />
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

      <SnapshotDialog
        sandboxId={sandbox.id}
        sandboxKey={sandbox.key}
        serverUrl={serverUrl}
        gatewayUrl={gatewayUrl}
        open={showSnapshot}
        onOpenChange={setShowSnapshot}
      />
    </div>
  );
}
