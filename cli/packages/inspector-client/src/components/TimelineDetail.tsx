import React, { memo, useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowLeft,
  ArrowRight,
  ChevronDown,
  ChevronRight,
  Maximize2,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { WsChunkRow } from "./WsChunkRow";
import type { SandboxEvent, SandboxTarget } from "@/types";
import type { TimelineBar } from "./TimelineView";
import { CodeViewer } from "./CodeViewer";
import { SegmentedControl } from "./SegmentedControl";
import { humanDuration } from "@/lib/utils";
import { LLM_PROVIDERS } from "@/lib/llmProviders";
import type { LLMSummaryData } from "@/lib/llmProviders";
import { LLMSummary } from "./LLMSummary";
import { tryPretty } from "@/lib/prettyBody";

type ConfigUpdater = (cfg: Record<string, unknown>) => Record<string, unknown>;

function getHeader(
  headers: Record<string, string> | undefined,
  name: string,
): string | undefined {
  if (!headers) return undefined;
  const lower = name.toLowerCase();
  for (const [k, v] of Object.entries(headers)) {
    if (k.toLowerCase() === lower) return v;
  }
}

function contentTypeToLang(ct: string): string {
  const base = ct.split(";")[0].trim().toLowerCase();
  if (base === "application/json" || base === "text/json" || base.endsWith("+json"))
    return "json";
  if (base === "text/html") return "html";
  if (base === "application/xml" || base === "text/xml" || base.endsWith("+xml"))
    return "xml";
  if (base === "text/css") return "css";
  if (
    base === "text/javascript" ||
    base === "application/javascript" ||
    base === "application/x-javascript" ||
    base === "text/ecmascript" ||
    base === "application/ecmascript"
  )
    return "javascript";
  if (base === "text/markdown" || base === "text/x-markdown") return "markdown";
  return "text";
}

type DetailTab = "summary" | "request" | "response";

// ─── generic helpers ────────────────────────────────────────────────────────

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
  if (bytes < 1024 * 1024 * 1024)
    return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)}GB`;
}

function KV({
  label,
  value,
  cls,
}: {
  label: string;
  value: string;
  cls?: string;
}) {
  return (
    <>
      <span className="text-muted-foreground/70 select-text">{label}</span>
      <span className={`font-mono break-all select-text ${cls ?? ""}`}>
        {value}
      </span>
    </>
  );
}

const HEADER_TRUNCATE = 100;

function TruncatedValue({ value }: { value: string }) {
  const [expanded, setExpanded] = useState(false);
  const needsTrunc = value.length > HEADER_TRUNCATE;
  if (!needsTrunc)
    return <span className="font-mono break-all select-text">{value}</span>;
  return (
    <button
      type="button"
      onClick={() => setExpanded((v) => !v)}
      className="font-mono break-all select-text text-left hover:opacity-80 transition-opacity"
    >
      {expanded ? value : `${value.slice(0, HEADER_TRUNCATE)}…`}
    </button>
  );
}

function AccessCell({
  access,
  applyConfig,
  allowUpdater,
  denyUpdater,
  target,
}: {
  access: "allowed" | "denied";
  applyConfig?: (
    updater: ConfigUpdater,
    target?: SandboxTarget,
  ) => Promise<void>;
  allowUpdater: ConfigUpdater;
  denyUpdater: ConfigUpdater;
  // Sandbox the event belongs to, so the policy edit is routed to it rather
  // than the primary sandbox (events may come from linked sandboxes).
  target?: SandboxTarget;
}) {
  const [applying, setApplying] = useState<"allow" | "deny" | null>(null);

  async function handleAction(action: "allow" | "deny") {
    if (!applyConfig || applying) return;
    setApplying(action);
    try {
      await applyConfig(action === "allow" ? allowUpdater : denyUpdater, target);
    } finally {
      setApplying(null);
    }
  }

  return (
    <div className="flex items-center gap-2">
      <span
        className={`font-mono break-all select-text ${access === "denied" ? "text-red-600 dark:text-red-400" : "text-green-600 dark:text-green-400"}`}
      >
        {access}
      </span>
      {applyConfig &&
        (access === "denied" ? (
          <button
            onClick={() => void handleAction("allow")}
            disabled={!!applying}
            className="text-[10px] px-1.5 py-0.5 rounded border border-green-700/40 text-green-700 hover:bg-green-700/10 dark:border-green-600/40 dark:text-green-500 dark:hover:bg-green-500/10 transition-colors disabled:opacity-40 font-mono"
          >
            {applying === "allow" ? "…" : "allow"}
          </button>
        ) : (
          <button
            onClick={() => void handleAction("deny")}
            disabled={!!applying}
            className="text-[10px] px-1.5 py-0.5 rounded border border-red-700/40 text-red-700 hover:bg-red-700/10 dark:border-red-600/40 dark:text-red-400 dark:hover:bg-red-500/10 transition-colors disabled:opacity-40 font-mono"
          >
            {applying === "deny" ? "…" : "deny"}
          </button>
        ))}
    </div>
  );
}

function egressRuleUpdater(
  host: string,
  path: string,
  access: "allow" | "deny",
): ConfigUpdater {
  return (cfg) => {
    const egress =
      (cfg.egress as Array<Record<string, unknown>> | undefined) ?? [];
    const pathsKey = JSON.stringify([path]);
    const first = egress[0];
    if (
      first?.host === host &&
      JSON.stringify(first.paths) === pathsKey &&
      first.access === access
    )
      return cfg;
    const filtered = egress.filter(
      (r) => !(r.host === host && JSON.stringify(r.paths) === pathsKey),
    );
    return { ...cfg, egress: [{ access, host, paths: [path] }, ...filtered] };
  };
}

function fsAclUpdater(
  mount: string,
  path: string,
  access: "rw" | "deny",
): ConfigUpdater {
  return (cfg) => {
    const fs = (cfg.fs as Array<Record<string, unknown>> | undefined) ?? [];
    return {
      ...cfg,
      fs: fs.map((entry) => {
        if (entry.mount !== mount) return entry;
        const acls =
          (entry.acls as Array<Record<string, unknown>> | undefined) ?? [];
        const existing = acls.find((a) => a.path === path);
        // If there's an existing rule for this exact path with the opposite access, remove it
        if (existing && existing.access !== access) {
          return { ...entry, acls: acls.filter((a) => a.path !== path) };
        }
        // No existing rule (or same access already) — prepend the new rule
        return {
          ...entry,
          acls: [{ path, access }, ...acls.filter((a) => a.path !== path)],
        };
      }),
    };
  };
}

function fsAllowUpdater(mount: string, path: string): ConfigUpdater {
  return fsAclUpdater(mount, path, "rw");
}

function fsDenyUpdater(mount: string, path: string): ConfigUpdater {
  return fsAclUpdater(mount, path, "deny");
}

function HeadersBlock({
  headers,
  className,
}: {
  headers?: Record<string, string>;
  className?: string;
}) {
  const [open, setOpen] = useState(
    () => localStorage.getItem("timeline:headersOpen") === "true",
  );

  useEffect(() => {
    localStorage.setItem("timeline:headersOpen", String(open));
  }, [open]);

  if (!headers || Object.keys(headers).length === 0) return null;
  const sorted = Object.entries(headers).sort(([a], [b]) => a.localeCompare(b));
  return (
    <div className={className}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1 text-[10px] uppercase tracking-wider text-muted-foreground/50 font-medium hover:text-muted-foreground/80 transition-colors"
      >
        {open ? (
          <ChevronDown className="h-3 w-3" />
        ) : (
          <ChevronRight className="h-3 w-3" />
        )}
        headers ({sorted.length})
      </button>
      {open && (
        <div className="mt-1.5 grid grid-cols-2 gap-x-3 gap-y-0.5">
          {sorted.map(([k, v]) => (
            <React.Fragment key={k}>
              <span className="text-muted-foreground/70 select-text">
                {k.toLowerCase()}
              </span>
              <TruncatedValue value={v} />
            </React.Fragment>
          ))}
        </div>
      )}
    </div>
  );
}

function BodyBlock({
  raw,
  contentType,
  className,
}: {
  raw?: string;
  contentType?: string;
  className?: string;
}) {
  const parsed = useMemo(() => tryPretty(raw), [raw]);
  if (!parsed) return null;
  const ctLang = contentType ? contentTypeToLang(contentType) : null;
  const lang =
    ctLang && ctLang !== "text" ? ctLang : parsed.isJson ? "json" : "text";
  return (
    <div
      className={`flex flex-col bg-muted/20 overflow-hidden ${className ?? ""}`}
    >
      <div className="px-3 py-1.5 text-[10px] uppercase tracking-wider text-muted-foreground/50 font-medium border-b border-border shrink-0 bg-background">
        Body
      </div>
      <CodeViewer
        content={parsed.content}
        lang={lang}
        className="flex-1 min-h-0"
      />
    </div>
  );
}

// ─── main detail panel ───────────────────────────────────────────────────────

// Prev/next navigation plus the expand control, shared by every event type.
// `pr-8` in the expanded view keeps the buttons clear of the dialog's close (×).
function DetailNav({
  onPrev,
  onNext,
  onExpand,
  expandedView,
}: {
  onPrev?: () => void;
  onNext?: () => void;
  onExpand?: () => void;
  expandedView?: boolean;
}) {
  return (
    <div
      className={`ml-auto flex items-center gap-2 ${expandedView ? "pr-8" : ""}`}
    >
      <Button size="sm" variant="ghost" onClick={onPrev} disabled={!onPrev}>
        <ArrowLeft className="h-3.5 w-3.5" />
      </Button>
      <Button size="sm" variant="ghost" onClick={onNext} disabled={!onNext}>
        <ArrowRight className="h-3.5 w-3.5" />
      </Button>
      {!expandedView && onExpand && (
        <Button size="sm" variant="ghost" onClick={onExpand} title="Expand">
          <Maximize2 className="h-3.5 w-3.5" />
        </Button>
      )}
    </div>
  );
}

function RowDetailPanelInner({
  bar,
  prevBar,
  onPrev,
  onNext,
  onExpand,
  applyConfig,
  onOpenFile,
  expandedView,
}: {
  bar: TimelineBar;
  prevBar?: TimelineBar | null;
  onPrev?: () => void;
  onNext?: () => void;
  onExpand?: () => void;
  applyConfig?: (
    updater: ConfigUpdater,
    target?: SandboxTarget,
  ) => Promise<void>;
  onOpenFile?: (path: string) => void;
  expandedView?: boolean;
}) {
  const req = bar.rawEvents[0];
  // Route policy edits to the sandbox that emitted this event. Falls back to
  // the primary sandbox when the event predates id/key tagging.
  const target: SandboxTarget | undefined =
    req?.sandbox_id && req.sandbox_key
      ? { id: req.sandbox_id, key: req.sandbox_key }
      : undefined;
  const res = bar.rawEvents.find(
    (
      e,
    ): e is Extract<
      SandboxEvent,
      {
        type:
          | "egress.response"
          | "ingress.response"
          | "fs.response"
          | "exec.response";
      }
    > =>
      e.type === "egress.response" ||
      e.type === "ingress.response" ||
      e.type === "fs.response" ||
      e.type === "exec.response",
  );
  const chunks = bar.rawEvents.filter(
    (e): e is Extract<SandboxEvent, { type: "egress.chunk" }> =>
      e.type === "egress.chunk",
  );

  const effectiveDurationMs = useMemo(() => {
    const lastChunk = chunks[chunks.length - 1];
    if (lastChunk) {
      return Math.round(
        new Date(lastChunk.timestamp).getTime() -
          new Date(req.timestamp).getTime(),
      );
    }
    if (res && "duration_ms" in res) return res.duration_ms;
    if (res)
      return Math.round(
        new Date(res.timestamp).getTime() - new Date(req.timestamp).getTime(),
      );
    return null;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bar]);

  const summaryData = useMemo((): LLMSummaryData | null => {
    if (req.type !== "egress.request") return null;
    const egressRes = res?.type === "egress.response" ? res : undefined;
    for (const provider of LLM_PROVIDERS) {
      const data = provider.parseSummary(req, egressRes, chunks);
      if (data) return data;
    }
    return null;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bar]);

  const prevSummaryData = useMemo((): LLMSummaryData | null => {
    if (!prevBar) return null;
    const prevReq = prevBar.rawEvents[0];
    if (prevReq.type !== "egress.request") return null;
    const prevRes = prevBar.rawEvents.find(
      (e): e is Extract<SandboxEvent, { type: "egress.response" }> =>
        e.type === "egress.response",
    );
    const prevChunks = prevBar.rawEvents.filter(
      (e): e is Extract<SandboxEvent, { type: "egress.chunk" }> =>
        e.type === "egress.chunk",
    );
    for (const provider of LLM_PROVIDERS) {
      const data = provider.parseSummary(prevReq, prevRes, prevChunks);
      if (data) {
        // Inject the previous response as an assistant turn so shared-message
        // detection correctly marks context carried forward into the current request.
        if (data.response && data.response.blocks.length > 0) {
          return {
            ...data,
            messages: [
              ...data.messages,
              { role: "assistant", content: data.response.blocks },
            ],
          };
        }
        return data;
      }
    }
    return null;
  }, [prevBar]);

  const hasSummary = summaryData !== null;

  const isWebSocket = useMemo(() => {
    if (!res || res.type !== "egress.response") return false;
    if (res.status === 101) return true;
    const upgrade = res.headers?.["upgrade"] ?? res.headers?.["Upgrade"] ?? "";
    return upgrade.toLowerCase().includes("websocket");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bar]);

  const [tab, setTab] = useState<DetailTab>(() => {
    const saved = localStorage.getItem(
      "timeline:detailTab",
    ) as DetailTab | null;
    if (saved === "summary") return hasSummary ? "summary" : "request";
    if (saved === "response") return "response";
    return hasSummary ? "summary" : "request";
  });

  useEffect(() => {
    localStorage.setItem("timeline:detailTab", tab);
  }, [tab]);

  const containerRef = useRef<HTMLDivElement>(null);
  const [narrow, setNarrow] = useState(false);
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => {
      setNarrow(entry.contentRect.width < 600);
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const ts = new Date(req.timestamp).toISOString().slice(11, 23);

  if (req.type === "stdio") {
    const text = (req.stdout ?? req.stderr ?? "").trimEnd();
    return (
      <div ref={containerRef} className="flex flex-col h-full text-xs">
        <div className="relative shrink-0">
          <div className="flex items-center gap-2 px-3 py-2">
            <span className="text-[10px] uppercase tracking-wider text-muted-foreground font-semibold">
              {req.stderr ? "stderr" : "stdout"}
            </span>
            <DetailNav
              onPrev={onPrev}
              onNext={onNext}
              onExpand={onExpand}
              expandedView={expandedView}
            />
          </div>
          <div className="pointer-events-none absolute inset-y-0 right-0 w-8 bg-gradient-to-l from-background to-transparent" />
        </div>
        <div
          className={`flex rounded-md border border-border mx-3 mb-3 flex-1 min-h-0 ${narrow ? "flex-col overflow-auto" : "overflow-hidden"}`}
        >
          <div
            className={`overflow-hidden shrink-0 ${narrow ? "border-b border-border" : "w-48 border-r border-border"}`}
          >
            <div className="p-3">
              <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5">
                <KV label="id" value={String(req.id)} />
                <KV label="time" value={ts} />
                <KV label="stream" value={req.stderr ? "stderr" : "stdout"} />
              </div>
            </div>
          </div>
          <CodeViewer content={text} className="flex-1 min-h-0" />
        </div>
      </div>
    );
  }

  if (req.type === "exec.request") {
    const durationMs = res
      ? Math.round(
          new Date(res.timestamp).getTime() - new Date(req.timestamp).getTime(),
        )
      : null;
    return (
      <div ref={containerRef} className="flex flex-col h-full text-xs">
        <div className="relative shrink-0">
          <div className="flex items-center gap-2 px-3 py-2">
            <span className="text-[10px] uppercase tracking-wider text-muted-foreground font-semibold">
              exec
            </span>
            <DetailNav
              onPrev={onPrev}
              onNext={onNext}
              onExpand={onExpand}
              expandedView={expandedView}
            />
          </div>
          <div className="pointer-events-none absolute inset-y-0 right-0 w-8 bg-gradient-to-l from-background to-transparent" />
        </div>
        <div
          className={`flex rounded-md border border-border mx-3 mb-3 flex-1 min-h-0 ${narrow ? "flex-col overflow-auto" : "overflow-hidden"}`}
        >
          <div
            className={`overflow-hidden shrink-0 ${narrow ? "border-b border-border" : "w-48 border-r border-border"}`}
          >
            <div className="p-3">
              <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5">
                <KV label="id" value={String(req.id)} />
                <KV label="time" value={ts} />
                <KV label="cwd" value={req.cwd} />
                {durationMs != null && (
                  <KV label="duration" value={humanDuration(durationMs)} />
                )}
              </div>
            </div>
          </div>
          <CodeViewer content={req.command} className="flex-1 min-h-0" />
        </div>
      </div>
    );
  }

  if (req.type === "resource.usage") {
    return (
      <div ref={containerRef} className="flex flex-col h-full text-xs">
        <div className="relative shrink-0">
          <div className="flex items-center gap-2 px-3 py-2">
            <span className="text-[10px] uppercase tracking-wider text-muted-foreground font-semibold">
              resource usage · {ts}
            </span>
            <DetailNav
              onPrev={onPrev}
              onNext={onNext}
              onExpand={onExpand}
              expandedView={expandedView}
            />
          </div>
        </div>
        <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5 px-3 pb-3 font-mono text-xs">
          <span className="text-muted-foreground/70">cpu</span>
          <span className="text-foreground">{req.cpu_percent.toFixed(1)}%</span>
          <span className="text-muted-foreground/70">memory</span>
          <span className="text-foreground">
            {formatBytes(req.memory_bytes)}
          </span>
        </div>
      </div>
    );
  }

  const reqRawBody =
    req.type === "egress.request" || req.type === "ingress.request"
      ? req.body
      : undefined;
  const resRawBody = res?.type === "ingress.response" ? res.body : undefined;

  const tabOptions: { value: DetailTab; label: string }[] = [
    ...(hasSummary
      ? [{ value: "summary" as DetailTab, label: "Summary" }]
      : []),
    { value: "request", label: "Request" },
    { value: "response", label: "Response" },
  ];

  return (
    <div ref={containerRef} className="flex flex-col h-full text-xs">
      <div className="relative shrink-0">
        <div className="detail-header-scroll overflow-x-auto">
          <div className="flex items-center gap-2 px-3 py-2 min-w-max">
            <SegmentedControl
              options={tabOptions}
              value={tab}
              onChange={setTab}
            />
            {hasSummary && summaryData && tab === "summary" && (
              <div className="ml-auto flex items-center gap-2">
                {summaryData.model && (
                  <span className="font-mono text-[10px] bg-muted/50 rounded px-1.5 py-0.5 text-muted-foreground text-nowrap">
                    {summaryData.model}
                  </span>
                )}
                {(summaryData.usage?.inputTokens != null ||
                  summaryData.usage?.outputTokens != null) && (
                  <span className="font-mono text-[10px] text-muted-foreground/60 text-nowrap">
                    {summaryData.usage.inputTokens ?? "?"}↑{" "}
                    {summaryData.usage.outputTokens ?? "?"}↓
                  </span>
                )}
                {bar.pending && (
                  <span className="font-mono text-[10px] text-blue-600/70 dark:text-blue-400/70">
                    streaming…
                  </span>
                )}
              </div>
            )}
            <DetailNav
              onPrev={onPrev}
              onNext={onNext}
              onExpand={onExpand}
              expandedView={expandedView}
            />
          </div>
        </div>
        <div className="pointer-events-none absolute inset-y-0 right-0 w-8 bg-gradient-to-l from-background to-transparent" />
      </div>

      {tab === "summary" && summaryData && (
        <LLMSummary summary={summaryData} prevSummary={prevSummaryData} />
      )}

      {tab === "request" && (
        <div
          className={`flex flex-1 min-h-0 rounded-md border border-border mx-3 mb-3 ${narrow ? "flex-col overflow-auto" : "overflow-hidden"}`}
        >
          <div
            className={`overflow-y-auto ${!narrow && reqRawBody ? "flex-1 min-w-0 border-r border-border" : narrow && reqRawBody ? "shrink-0 border-b border-border" : "flex-1"}`}
          >
            <div className="p-3">
              <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5">
                <KV label="id" value={String(req.id)} />
                <KV label="time" value={ts} />
                {req.type === "egress.request" && (
                  <>
                    <KV label="method" value={req.method} />
                    <KV label="host" value={req.host} />
                    <KV label="path" value={req.path} />
                    {req.query && <KV label="query" value={req.query} />}
                    <span className="text-muted-foreground/70 select-text">
                      access
                    </span>
                    <AccessCell
                      access={req.access}
                      applyConfig={applyConfig}
                      target={target}
                      allowUpdater={egressRuleUpdater(
                        req.host,
                        req.path,
                        "allow",
                      )}
                      denyUpdater={egressRuleUpdater(
                        req.host,
                        req.path,
                        "deny",
                      )}
                    />
                    {req.headers && (
                      <div className="col-span-2 mt-1">
                        <HeadersBlock headers={req.headers} />
                      </div>
                    )}
                  </>
                )}
                {req.type === "fs.request" && (
                  <>
                    <KV label="op" value={req.operation} />
                    <span className="text-muted-foreground/70 select-text">
                      path
                    </span>
                    {onOpenFile &&
                    (req.operation === "read" || req.operation === "write") ? (
                      <button
                        className="font-mono break-all select-text text-left"
                        onClick={() => onOpenFile(req.path)}
                      >
                        {req.path}
                      </button>
                    ) : (
                      <span className="font-mono break-all select-text">
                        {req.path}
                      </span>
                    )}
                    <KV label="mount" value={req.mount} />
                    <span className="text-muted-foreground/70 select-text">
                      access
                    </span>
                    <AccessCell
                      access={req.access}
                      applyConfig={applyConfig}
                      target={target}
                      allowUpdater={fsAllowUpdater(req.mount, req.path)}
                      denyUpdater={fsDenyUpdater(req.mount, req.path)}
                    />
                  </>
                )}
                {req.type === "ingress.request" && (
                  <>
                    <KV label="method" value={req.method} />
                    <KV label="port" value={req.port} />
                    <KV label="path" value={req.path} />
                    {req.query && <KV label="query" value={req.query} />}
                    {req.headers && (
                      <div className="col-span-2 mt-1">
                        <HeadersBlock headers={req.headers} />
                      </div>
                    )}
                  </>
                )}
              </div>
            </div>
          </div>
          {reqRawBody && (
            <BodyBlock
              raw={reqRawBody}
              contentType={getHeader(
                req.type === "egress.request" || req.type === "ingress.request"
                  ? req.headers
                  : undefined,
                "content-type",
              )}
              className={
                narrow ? "flex-1 min-h-[300px]" : "flex-1 min-w-0 min-h-0"
              }
            />
          )}
        </div>
      )}

      {tab === "response" && (
        <div
          className={`flex flex-1 min-h-0 rounded-md border border-border mx-3 mb-3 ${narrow ? "flex-col overflow-auto" : "overflow-hidden"}`}
        >
          <div
            className={`overflow-y-auto ${!narrow && (chunks.length > 0 || resRawBody) ? "flex-1 min-w-0 border-r border-border" : narrow && (chunks.length > 0 || resRawBody) ? "shrink-0 border-b border-border" : "flex-1"}`}
          >
            <div className="p-3">
              {res ? (
                <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5">
                  {(res.type === "egress.response" ||
                    res.type === "ingress.response") && (
                    <>
                      <KV
                        label="status"
                        value={String(res.status)}
                        cls={
                          res.status >= 400
                            ? "text-red-600 dark:text-red-400"
                            : "text-green-600 dark:text-green-400"
                        }
                      />
                      {effectiveDurationMs != null && (
                        <KV
                          label="duration"
                          value={humanDuration(effectiveDurationMs)}
                        />
                      )}
                      {res.headers && (
                        <div className="col-span-2 mt-1">
                          <HeadersBlock headers={res.headers} />
                        </div>
                      )}
                    </>
                  )}
                  {res.type === "fs.response" && (
                    <>
                      <KV label="backend" value={res.backend} />
                      {effectiveDurationMs != null && (
                        <KV
                          label="duration"
                          value={humanDuration(effectiveDurationMs)}
                        />
                      )}
                      {res.error && (
                        <KV
                          label="error"
                          value={res.error}
                          cls="text-red-600 dark:text-red-400"
                        />
                      )}
                    </>
                  )}
                </div>
              ) : (
                <span className="text-muted-foreground">
                  {bar.pending ? "awaiting response…" : "no response received"}
                </span>
              )}
            </div>
          </div>
          {chunks.length > 0 && (
            <div
              className={`flex flex-col bg-muted/20 overflow-hidden ${narrow ? "flex-1 min-h-[300px]" : "flex-1 min-w-0 min-h-0"}`}
            >
              <div className="px-3 py-1.5 text-[10px] uppercase tracking-wider text-muted-foreground/50 font-medium border-b border-border shrink-0 bg-background">
                Body
              </div>
              {isWebSocket ? (
                <div className="flex flex-1 min-h-0 flex-col overflow-y-auto py-1">
                  {chunks.map((chunk) => (
                    <WsChunkRow key={chunk.id} chunk={chunk} />
                  ))}
                </div>
              ) : (
                (() => {
                  const raw = chunks.map((c) => c.body).join("\n\n");
                  const pretty = tryPretty(raw);
                  const resContentType = getHeader(
                    res?.type === "egress.response" || res?.type === "ingress.response"
                      ? res.headers
                      : undefined,
                    "content-type",
                  );
                  const ctLang = resContentType
                    ? contentTypeToLang(resContentType)
                    : null;
                  const lang =
                    ctLang && ctLang !== "text"
                      ? ctLang
                      : pretty?.isJson
                        ? "json"
                        : "text";
                  return (
                    <CodeViewer
                      content={pretty?.content ?? raw}
                      lang={lang}
                      className="flex-1 min-h-0"
                    />
                  );
                })()
              )}
            </div>
          )}
          {resRawBody && (
            <BodyBlock
              raw={resRawBody}
              contentType={getHeader(
                res?.type === "ingress.response" ? res.headers : undefined,
                "content-type",
              )}
              className={
                narrow ? "flex-1 min-h-[300px]" : "flex-1 min-w-0 min-h-0"
              }
            />
          )}
        </div>
      )}
    </div>
  );
}

// Memoized so the timeline's once-per-frame re-render during event streaming
// doesn't touch this panel's DOM while the selected event is unchanged —
// re-rendering it replaced text nodes and wiped the user's text selection.
// Effective because TimelineView keeps `bar`/`prevBar` referentially stable
// while their content is unchanged and passes stable handler identities (see
// useStableBar / the ref-backed nav callbacks there).
export const RowDetailPanel = memo(RowDetailPanelInner);
