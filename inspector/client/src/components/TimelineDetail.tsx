import React, { useEffect, useMemo, useRef, useState } from "react";
import { ArrowDown, ArrowUp, ChevronDown, ChevronRight, History } from "lucide-react";
import { Button } from "@/components/ui/button";
import { WsChunkRow } from "./WsChunkRow";
import type { ReactNode } from "react";
import type { SandboxEvent } from "@/types";
import type { TimelineBar } from "./TimelineView";
import { CodeViewer } from "./CodeViewer";
import { SegmentedControl } from "./SegmentedControl";
import { humanDuration } from "@/lib/utils";
import { LLM_PROVIDERS } from "@/lib/llmProviders";
import type { LLMSummaryData, LLMContentBlock } from "@/lib/llmProviders";

type ConfigUpdater = (cfg: Record<string, unknown>) => Record<string, unknown>;

type DetailTab = "summary" | "request" | "response";

// ─── generic helpers ────────────────────────────────────────────────────────

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)}GB`;
}

function tryPretty(body?: string): { content: string; isJson: boolean } | undefined {
  if (!body) return undefined;
  try { return { content: JSON.stringify(JSON.parse(body), null, 2), isJson: true }; }
  catch { return { content: body, isJson: false }; }
}

function KV({ label, value, cls }: { label: string; value: string; cls?: string }) {
  return (
    <>
      <span className="text-muted-foreground/70 select-text">{label}</span>
      <span className={`font-mono break-all select-text ${cls ?? ""}`}>{value}</span>
    </>
  );
}

const HEADER_TRUNCATE = 100;

function TruncatedValue({ value }: { value: string }) {
  const [expanded, setExpanded] = useState(false);
  const needsTrunc = value.length > HEADER_TRUNCATE;
  if (!needsTrunc) return <span className="font-mono break-all select-text">{value}</span>;
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
}: {
  access: "allowed" | "denied";
  applyConfig?: (updater: ConfigUpdater) => Promise<void>;
  allowUpdater: ConfigUpdater;
  denyUpdater: ConfigUpdater;
}) {
  const [applying, setApplying] = useState<"allow" | "deny" | null>(null);

  async function handleAction(action: "allow" | "deny") {
    if (!applyConfig || applying) return;
    setApplying(action);
    try {
      await applyConfig(action === "allow" ? allowUpdater : denyUpdater);
    } finally {
      setApplying(null);
    }
  }

  return (
    <div className="flex items-center gap-2">
      <span className={`font-mono break-all select-text ${access === "denied" ? "text-red-400" : "text-green-400"}`}>
        {access}
      </span>
      {applyConfig && (
        access === "denied" ? (
          <button
            onClick={() => void handleAction("allow")}
            disabled={!!applying}
            className="text-[10px] px-1.5 py-0.5 rounded border border-green-600/40 text-green-500 hover:bg-green-500/10 transition-colors disabled:opacity-40 font-mono"
          >
            {applying === "allow" ? "…" : "allow"}
          </button>
        ) : (
          <button
            onClick={() => void handleAction("deny")}
            disabled={!!applying}
            className="text-[10px] px-1.5 py-0.5 rounded border border-red-600/40 text-red-400 hover:bg-red-500/10 transition-colors disabled:opacity-40 font-mono"
          >
            {applying === "deny" ? "…" : "deny"}
          </button>
        )
      )}
    </div>
  );
}

function egressRuleUpdater(host: string, path: string, access: "allow" | "deny"): ConfigUpdater {
  return (cfg) => {
    const egress = (cfg.egress as Array<Record<string, unknown>> | undefined) ?? [];
    const pathsKey = JSON.stringify([path]);
    const first = egress[0];
    if (first?.host === host && JSON.stringify(first.paths) === pathsKey && first.access === access) return cfg;
    const filtered = egress.filter((r) => !(r.host === host && JSON.stringify(r.paths) === pathsKey));
    return { ...cfg, egress: [{ access, host, paths: [path] }, ...filtered] };
  };
}

function fsAclUpdater(mount: string, path: string, access: "rw" | "deny"): ConfigUpdater {
  return (cfg) => {
    const fs = (cfg.fs as Array<Record<string, unknown>> | undefined) ?? [];
    return {
      ...cfg,
      fs: fs.map((entry) => {
        if (entry.mount !== mount) return entry;
        const acls = (entry.acls as Array<Record<string, unknown>> | undefined) ?? [];
        const existing = acls.find((a) => a.path === path);
        // If there's an existing rule for this exact path with the opposite access, remove it
        if (existing && existing.access !== access) {
          return { ...entry, acls: acls.filter((a) => a.path !== path) };
        }
        // No existing rule (or same access already) — prepend the new rule
        return { ...entry, acls: [{ path, access }, ...acls.filter((a) => a.path !== path)] };
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

function HeadersBlock({ headers, className }: { headers?: Record<string, string>; className?: string }) {
  const [open, setOpen] = useState(() => localStorage.getItem("timeline:headersOpen") === "true");

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
        {open ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
        headers ({sorted.length})
      </button>
      {open && (
        <div className="mt-1.5 grid grid-cols-2 gap-x-3 gap-y-0.5">
          {sorted.map(([k, v]) => (
            <React.Fragment key={k}>
              <span className="text-muted-foreground/70 select-text">{k.toLowerCase()}</span>
              <TruncatedValue value={v} />
            </React.Fragment>
          ))}
        </div>
      )}
    </div>
  );
}

function BodyBlock({ raw, className }: { raw?: string; className?: string }) {
  const parsed = useMemo(() => tryPretty(raw), [raw]);
  if (!parsed) return null;
  return (
    <div className={`flex flex-col rounded-md border border-border bg-muted/20 overflow-hidden min-h-0 ${className ?? ""}`}>
      <div className="flex-1 min-h-0">
        <CodeViewer content={parsed.content} lang={parsed.isJson ? "json" : "text"} className="h-full" />
      </div>
    </div>
  );
}

function prettyJson(s?: string): string {
  if (!s) return "";
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

// ─── summary rendering ───────────────────────────────────────────────────────

function Bubble({ role, children, defaultCollapsed = false, repeated = false }: { role: string; children: ReactNode; defaultCollapsed?: boolean; repeated?: boolean }) {
  const roleColor: Record<string, string> = {
    user: "text-green-400",
    assistant: "text-blue-400",
    system: "text-yellow-500",
    tool: "text-purple-400",
  };
  const [collapsed, setCollapsed] = useState(defaultCollapsed);
  return (
    <div className="flex flex-col rounded-md bg-muted/20 px-2.5 py-2 gap-2">
      <button
        className={`flex items-center gap-1 text-[10px] uppercase font-semibold tracking-wider w-fit ${roleColor[role] ?? "text-muted-foreground"}`}
        onClick={() => setCollapsed((v) => !v)}
      >
        {collapsed ? <ChevronRight className="h-3 w-3" /> : <ChevronDown className="h-3 w-3" />}
        {role}
        {repeated && (
          <span className="group/hist inline-flex items-center ml-1">
            <History className="h-3 w-3 text-muted-foreground/40" />
            <span className="ml-1 text-[10px] font-normal normal-case tracking-normal text-muted-foreground/40 opacity-0 group-hover/hist:opacity-100 transition-opacity duration-150">
              previous context
            </span>
          </span>
        )}
      </button>
      {!collapsed && (
        <div className="px-2.5 py-2 flex flex-col gap-2">
          {children}
        </div>
      )}
    </div>
  );
}

function PlainText({ text }: { text: string }) {
  return (
    <p className="font-mono text-xs whitespace-pre-wrap break-all text-foreground/90 select-text">{text}</p>
  );
}

function ToolUseBlock({ name, input }: { name: string; input?: unknown }) {
  const pretty = useMemo(() => prettyJson(input !== undefined ? JSON.stringify(input) : undefined), [input]);
  return (
    <div className="flex flex-col gap-1">
      <span className="font-mono text-[10px] text-purple-400 font-semibold">{name}</span>
      {pretty && (
        <div className="rounded border border-border overflow-hidden">
          <CodeViewer content={pretty} lang="json" minHeight={60} maxHeight={160} />
        </div>
      )}
    </div>
  );
}

function renderBlocks(blocks: LLMContentBlock[]): ReactNode {
  return blocks.map((blk, i) => {
    if (blk.type === "text" && blk.text)
      return <PlainText key={i} text={blk.text} />;
    if (blk.type === "tool_use")
      return <ToolUseBlock key={i} name={blk.toolName ?? "unknown"} input={blk.toolInput} />;
    if (blk.type === "tool_result") {
      const raw = typeof blk.toolResultContent === "string" ? blk.toolResultContent : JSON.stringify(blk.toolResultContent);
      const { content: pretty, isJson } = tryPretty(raw) ?? { content: raw ?? "", isJson: false };
      return (
        <div key={i} className="flex flex-col gap-1">
          <span className="font-mono text-[10px] text-purple-400/70">result · {blk.toolId}</span>
          <div className="rounded border border-border overflow-hidden">
            <CodeViewer content={pretty} lang={isJson ? "json" : "text"} minHeight={60} maxHeight={320} />
          </div>
        </div>
      );
    }
    return null;
  });
}

function SummaryTab({ summary, prevSummary }: { summary: LLMSummaryData; prevSummary?: LLMSummaryData | null }) {
  const sharedIndices = useMemo(() => {
    const current = summary.messages;
    const prev = prevSummary?.messages ?? [];
    const shared = new Set<number>();
    for (let i = 0; i < current.length && i < prev.length; i++) {
      if (JSON.stringify(current[i]) === JSON.stringify(prev[i])) shared.add(i);
    }
    return shared;
  }, [summary.messages, prevSummary?.messages]);

  return (
    <div className="flex flex-col gap-3 px-3 pb-3 overflow-y-auto flex-1 min-h-0">
      {summary.system && (
        <Bubble role="system" defaultCollapsed>
          <PlainText text={summary.system} />
        </Bubble>
      )}

      {summary.messages.map((msg, i) => (
        <Bubble key={i} role={msg.role} defaultCollapsed={sharedIndices.has(i)} repeated={sharedIndices.has(i)}>
          {renderBlocks(msg.content)}
        </Bubble>
      ))}

      {summary.response && summary.response.blocks.length > 0 && (
        <Bubble role="assistant">
          {renderBlocks(summary.response.blocks)}
        </Bubble>
      )}
    </div>
  );
}

// ─── main detail panel ───────────────────────────────────────────────────────


export function RowDetailPanel({ bar, prevBar, onPrev, onNext, applyConfig, onOpenFile }: { bar: TimelineBar; prevBar?: TimelineBar | null; onPrev?: () => void; onNext?: () => void; applyConfig?: (updater: ConfigUpdater) => Promise<void>; onOpenFile?: (path: string) => void }) {
  const req = bar.rawEvents[0];
  const res = bar.rawEvents.find(
    (e): e is Extract<SandboxEvent, { type: "egress.response" | "fs.response" }> =>
      e.type === "egress.response" || e.type === "fs.response",
  );
  const chunks = bar.rawEvents.filter(
    (e): e is Extract<SandboxEvent, { type: "egress.chunk" }> =>
      e.type === "egress.chunk",
  );

  const effectiveDurationMs = useMemo(() => {
    const lastChunk = chunks[chunks.length - 1];
    if (lastChunk) {
      return Math.round(new Date(lastChunk.timestamp).getTime() - new Date(req.timestamp).getTime());
    }
    return res && "duration_ms" in res ? res.duration_ms : null;
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
      (e): e is Extract<SandboxEvent, { type: "egress.response" }> => e.type === "egress.response",
    );
    const prevChunks = prevBar.rawEvents.filter(
      (e): e is Extract<SandboxEvent, { type: "egress.chunk" }> => e.type === "egress.chunk",
    );
    for (const provider of LLM_PROVIDERS) {
      const data = provider.parseSummary(prevReq, prevRes, prevChunks);
      if (data) {
        // Inject the previous response as an assistant turn so shared-message
        // detection correctly marks context carried forward into the current request.
        if (data.response && data.response.blocks.length > 0) {
          return {
            ...data,
            messages: [...data.messages, { role: "assistant", content: data.response.blocks }],
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
    const saved = localStorage.getItem("timeline:detailTab") as DetailTab | null;
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
      setNarrow(entry.contentRect.width < 700);
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const ts = new Date(req.timestamp).toISOString().slice(11, 23);

  if (req.type === "stdio") {
    const text = (req.stdout ?? req.stderr ?? "").trimEnd();
    return (
      <div className="flex flex-col h-full p-3 gap-2">
        <div className="text-[10px] uppercase tracking-wider text-muted-foreground font-semibold shrink-0">
          {req.stderr ? "stderr" : "stdout"} · {ts}
        </div>
        <div className="flex-1 min-h-0">
          <CodeViewer content={text} className="h-full" />
        </div>
      </div>
    );
  }

  if (req.type === "resource.usage") {
    return (
      <div className="flex flex-col h-full p-3 gap-2">
        <div className="text-[10px] uppercase tracking-wider text-muted-foreground font-semibold shrink-0">
          resource usage · {ts}
        </div>
        <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5 font-mono text-xs">
          <span className="text-muted-foreground/70">cpu</span>
          <span className="text-white">{req.cpu_percent.toFixed(1)}%</span>
          <span className="text-muted-foreground/70">memory</span>
          <span className="text-white">{formatBytes(req.memory_bytes)}</span>
        </div>
      </div>
    );
  }

  const reqRawBody = req.type === "egress.request" ? req.body : undefined;

  const tabOptions: { value: DetailTab; label: string }[] = [
    ...(hasSummary ? [{ value: "summary" as DetailTab, label: "Summary" }] : []),
    { value: "request", label: "Request" },
    { value: "response", label: "Response" },
  ];

  return (
    <div ref={containerRef} className="flex flex-col h-full text-xs">
      <div className="relative shrink-0">
        <div className="detail-header-scroll overflow-x-auto">
          <div className="flex items-center gap-2 px-3 py-2 min-w-max">
          <SegmentedControl options={tabOptions} value={tab} onChange={setTab} />
          <div className="ml-auto flex items-center gap-2">
            {hasSummary && summaryData && tab === "summary" && (
              <>
                {summaryData.model && (
                  <span className="font-mono text-[10px] bg-muted/50 rounded px-1.5 py-0.5 text-muted-foreground text-nowrap">
                    {summaryData.model}
                  </span>
                )}
                {summaryData.response?.stopReason && (
                  <span className="font-mono text-[10px] bg-muted/50 rounded px-1.5 py-0.5 text-muted-foreground text-nowrap">
                    {summaryData.response.stopReason}
                  </span>
                )}
                {(summaryData.usage?.inputTokens != null || summaryData.usage?.outputTokens != null) && (
                  <span className="font-mono text-[10px] text-muted-foreground/60 text-nowrap">
                    {summaryData.usage.inputTokens ?? "?"}↑ {summaryData.usage.outputTokens ?? "?"}↓
                  </span>
                )}
                {bar.pending && !summaryData.response?.stopReason && (
                  <span className="font-mono text-[10px] text-blue-400/70">streaming…</span>
                )}
              </>
            )}
            <Button size="sm" variant="ghost" onClick={onPrev} disabled={!onPrev}>
              <ArrowUp className="h-3.5 w-3.5" />
            </Button>
            <Button size="sm" variant="ghost" onClick={onNext} disabled={!onNext}>
              <ArrowDown className="h-3.5 w-3.5" />
            </Button>
          </div>
          </div>
        </div>
        <div className="pointer-events-none absolute inset-y-0 right-0 w-8 bg-gradient-to-l from-background to-transparent" />
      </div>

      {tab === "summary" && summaryData && (
        <SummaryTab summary={summaryData} prevSummary={prevSummaryData} />
      )}

      {tab === "request" && (
        <div className={`flex flex-1 min-h-0 gap-3 px-3 pb-3 ${narrow ? "flex-col overflow-y-auto" : ""}`}>
          <div className={`rounded-md border border-border overflow-hidden min-w-0 ${!narrow && reqRawBody ? "flex-1" : narrow ? "shrink-0" : "w-full"}`}>
            <div className={`p-3 overflow-auto ${narrow ? "" : "h-full"}`}>
              <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5">
                <KV label="id"   value={String(req.id)} />
                <KV label="time" value={ts} />
                {req.type === "egress.request" && <>
                  <KV label="method" value={req.method} />
                  <KV label="host"   value={req.host} />
                  <KV label="path"   value={req.path} />
                  {req.query && <KV label="query" value={req.query} />}
                  <span className="text-muted-foreground/70 select-text">access</span>
                  <AccessCell
                    access={req.access}
                    applyConfig={applyConfig}
                    allowUpdater={egressRuleUpdater(req.host, req.path, "allow")}
                    denyUpdater={egressRuleUpdater(req.host, req.path, "deny")}
                  />
                  {req.headers && <div className="col-span-2 mt-1"><HeadersBlock headers={req.headers} /></div>}
                </>}
                {req.type === "fs.request" && <>
                  <KV label="op"     value={req.operation} />
                  <span className="text-muted-foreground/70 select-text">path</span>
                  {onOpenFile && (req.operation === "read" || req.operation === "write") ? (
                    <button
                      className="font-mono break-all select-text text-left"
                      onClick={() => onOpenFile(req.path)}
                    >
                      {req.path}
                    </button>
                  ) : (
                    <span className="font-mono break-all select-text">{req.path}</span>
                  )}
                  <KV label="mount"  value={req.mount} />
                  <span className="text-muted-foreground/70 select-text">access</span>
                  <AccessCell
                    access={req.access}
                    applyConfig={applyConfig}
                    allowUpdater={fsAllowUpdater(req.mount, req.path)}
                    denyUpdater={fsDenyUpdater(req.mount, req.path)}
                  />
                </>}
              </div>
            </div>
          </div>
          {reqRawBody && (
            <BodyBlock raw={reqRawBody} className={narrow ? "flex-1 min-h-[400px]" : "flex-1 min-w-0"} />
          )}
        </div>
      )}

      {tab === "response" && (
        <div className={`flex flex-1 min-h-0 gap-3 px-3 pb-3 ${narrow ? "flex-col overflow-y-auto" : ""}`}>
          <div className={`rounded-md border border-border overflow-hidden min-w-0 ${!narrow && chunks.length > 0 ? "flex-1" : narrow ? "shrink-0" : "w-full"}`}>
            <div className={`p-3 overflow-auto ${narrow ? "" : "h-full"}`}>
              {res ? (
                <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5">
                  {res.type === "egress.response" && <>
                    <KV label="status"   value={String(res.status)} cls={res.status >= 400 ? "text-red-400" : "text-green-400"} />
                    {effectiveDurationMs != null && <KV label="duration" value={humanDuration(effectiveDurationMs)} />}
                    {res.headers && <div className="col-span-2 mt-1"><HeadersBlock headers={res.headers} /></div>}
                  </>}
                  {res.type === "fs.response" && <>
                    <KV label="backend"  value={res.backend} />
                    {effectiveDurationMs != null && <KV label="duration" value={humanDuration(effectiveDurationMs)} />}
                    {res.error && <KV label="error" value={res.error} cls="text-red-400" />}
                  </>}
                </div>
              ) : (
                <span className="text-muted-foreground">{bar.pending ? "awaiting response…" : "no response received"}</span>
              )}
            </div>
          </div>
          {chunks.length > 0 && (
            <div className={`flex min-w-0 flex-col gap-2 ${narrow ? "flex-1 min-h-[400px]" : "flex-1 min-h-0 overflow-hidden"}`}>
              {isWebSocket ? (
                <div className="flex flex-col flex-1 min-h-0 overflow-y-auto rounded-md border border-border bg-muted/20 py-1">
                  {chunks.map((chunk) => (
                    <WsChunkRow key={chunk.id} chunk={chunk} />
                  ))}
                </div>
              ) : (() => {
                const raw = chunks.map((c) => c.body).join("\n\n");
                const pretty = tryPretty(raw);
                return (
                  <div className="flex flex-1 min-h-0 flex-col rounded-md border border-border bg-muted/20 overflow-hidden">
                    <CodeViewer content={pretty?.content ?? raw} lang={pretty?.isJson ? "json" : "text"} className="flex-1 min-h-0" />
                  </div>
                );
              })()}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
