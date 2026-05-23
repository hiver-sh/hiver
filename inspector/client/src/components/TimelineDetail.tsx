import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, ChevronDown, ChevronRight, History } from "lucide-react";
import { Button } from "@/components/ui/button";
import type { ReactNode } from "react";
import type { SandboxEvent } from "@/types";
import type { TimelineRow } from "./TimelineView";
import { CodeViewer } from "./CodeViewer";
import { SegmentedControl } from "./SegmentedControl";

type ConfigUpdater = (cfg: Record<string, unknown>) => Record<string, unknown>;

type DetailTab = "summary" | "request" | "response";

// ─── generic helpers ────────────────────────────────────────────────────────

function tryPretty(body?: string): { content: string; isJson: boolean } | undefined {
  if (!body) return undefined;
  try { return { content: JSON.stringify(JSON.parse(body), null, 2), isJson: true }; }
  catch { return { content: body, isJson: false }; }
}

function egressMethodCls(method: string): string {
  switch (method) {
    case "GET":    return "text-green-400";
    case "POST":   return "text-blue-400";
    case "PUT":
    case "PATCH":  return "text-orange-400";
    case "DELETE": return "text-red-400";
    default:       return "text-muted-foreground";
  }
}

function KV({ label, value, cls }: { label: string; value: string; cls?: string }) {
  return (
    <>
      <span className="text-muted-foreground/70 select-text">{label}</span>
      <span className={`font-mono break-all select-text ${cls ?? ""}`}>{value}</span>
    </>
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
    const egress = (cfg.egress as unknown[] | undefined) ?? [];
    return { ...cfg, egress: [{ access, host, paths: [path] }, ...egress] };
  };
}

function fsAllowUpdater(mount: string, path: string): ConfigUpdater {
  return (cfg) => {
    const fs = (cfg.fs as Array<Record<string, unknown>> | undefined) ?? [];
    return {
      ...cfg,
      fs: fs.map((entry) => {
        if (entry.mount !== mount) return entry;
        const acls = (entry.acls as Array<Record<string, unknown>> | undefined) ?? [];
        return { ...entry, acls: [...acls.filter((a) => a.path !== path), { path, access: "rw" }] };
      }),
    };
  };
}

function fsDenyUpdater(mount: string, path: string): ConfigUpdater {
  return (cfg) => {
    const fs = (cfg.fs as Array<Record<string, unknown>> | undefined) ?? [];
    return {
      ...cfg,
      fs: fs.map((entry) => {
        if (entry.mount !== mount) return entry;
        const acls = (entry.acls as Array<Record<string, unknown>> | undefined) ?? [];
        return { ...entry, acls: [...acls.filter((a) => a.path !== path), { path, access: "deny" }] };
      }),
    };
  };
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

// ─── Anthropic /v1/messages summary ─────────────────────────────────────────

interface StreamBlock {
  type: "text" | "tool_use";
  text?: string;
  id?: string;
  name?: string;
  inputJson?: string;
}

interface StreamResult {
  blocks: StreamBlock[];
  stopReason?: string;
  inputTokens?: number;
  outputTokens?: number;
  model?: string;
}

function parseAnthropicStream(
  chunks: Extract<SandboxEvent, { type: "egress.stream_chunk" }>[],
): StreamResult {
  const blocks: StreamBlock[] = [];
  let stopReason: string | undefined;
  let inputTokens: number | undefined;
  let outputTokens: number | undefined;
  let model: string | undefined;

  // Each chunk is one SSE line (empty lines are dropped by the proxy).
  // Only data: lines carry JSON; skip everything else.
  for (const chunk of chunks) {
    if (!chunk.body.startsWith("data: ")) continue;
    const data = chunk.body.slice(6);
    if (!data || data === "[DONE]") continue;
    let msg: Record<string, unknown>;
    try { msg = JSON.parse(data) as Record<string, unknown>; } catch { continue; }

    switch (msg.type) {
      case "message_start": {
        const m = msg.message as Record<string, unknown> | undefined;
        if (m?.model) model = m.model as string;
        const u = m?.usage as Record<string, number> | undefined;
        if (u) { inputTokens = u.input_tokens; outputTokens = u.output_tokens; }
        break;
      }
      case "content_block_start": {
        const idx = msg.index as number;
        const cb = msg.content_block as Record<string, unknown>;
        if (cb.type === "text")
          blocks[idx] = { type: "text", text: (cb.text as string) ?? "" };
        else if (cb.type === "tool_use")
          blocks[idx] = { type: "tool_use", id: cb.id as string, name: cb.name as string, inputJson: "" };
        break;
      }
      case "content_block_delta": {
        const idx = msg.index as number;
        const delta = msg.delta as Record<string, unknown>;
        const blk = blocks[idx];
        if (!blk) break;
        if (delta.type === "text_delta" && blk.type === "text")
          blk.text = (blk.text ?? "") + (delta.text as string);
        else if (delta.type === "input_json_delta" && blk.type === "tool_use")
          blk.inputJson = (blk.inputJson ?? "") + (delta.partial_json as string);
        break;
      }
      case "message_delta": {
        const d = msg.delta as Record<string, unknown> | undefined;
        if (d?.stop_reason) stopReason = d.stop_reason as string;
        const u = msg.usage as Record<string, number> | undefined;
        if (u?.output_tokens) outputTokens = u.output_tokens;
        break;
      }
    }
  }

  return { blocks: blocks.filter(Boolean), stopReason, inputTokens, outputTokens, model };
}

type MsgContentPart =
  | { type: "text"; text?: string }
  | { type: "tool_use"; id?: string; name?: string; input?: unknown }
  | { type: "tool_result"; tool_use_id?: string; content?: unknown };

interface AnthropicReqBody {
  model?: string;
  system?: string | Array<{ type: string; text?: string }>;
  messages?: Array<{ role: string; content: string | MsgContentPart[] }>;
}

function parseAnthropicRequest(body?: string): AnthropicReqBody | null {
  if (!body) return null;
  try { return JSON.parse(body) as AnthropicReqBody; } catch { return null; }
}

function resolveSystem(system?: string | Array<{ type: string; text?: string }>): string | undefined {
  if (!system) return undefined;
  if (typeof system === "string") return system || undefined;
  return system.filter((b) => b.type === "text").map((b) => b.text ?? "").join("\n") || undefined;
}

function isThinkingBlock(v: unknown): boolean {
  if (!v || typeof v !== "object") return false;
  const t = (v as Record<string, unknown>).type;
  return t === "thinking" || t === "redacted_thinking";
}

function normalizeMsg(obj: unknown): unknown {
  if (Array.isArray(obj)) return obj.filter((v) => !isThinkingBlock(v)).map(normalizeMsg);
  if (obj && typeof obj === "object") {
    const entries = Object.entries(obj as Record<string, unknown>)
      .filter(([k]) => k !== "cache_control")
      .map(([k, v]): [string, unknown] => {
        if (k === "content" && typeof v === "string") return [k, [{ text: v, type: "text" }]];
        return [k, normalizeMsg(v)];
      })
      .sort(([a], [b]) => a.localeCompare(b));
    return Object.fromEntries(entries);
  }
  return obj;
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

function ToolUseBlock({ name, inputJson }: { name: string; inputJson?: string }) {
  const pretty = useMemo(() => prettyJson(inputJson), [inputJson]);
  return (
    <div className="flex flex-col gap-1">
      <span className="font-mono text-[10px] text-purple-400 font-semibold">{name}</span>
      {pretty && (
        <div className="rounded border border-border overflow-hidden">
          <CodeViewer content={pretty} lang="json" className="max-h-40" />
        </div>
      )}
    </div>
  );
}

function renderContent(content: string | MsgContentPart[]): ReactNode {
  if (typeof content === "string") return <PlainText text={content} />;
  return content.filter((part) => !isThinkingBlock(part)).map((part, i) => {
    if (part.type === "text" && part.text)
      return <PlainText key={i} text={part.text} />;
    if (part.type === "tool_use")
      return <ToolUseBlock key={i} name={(part.name as string | undefined) ?? "unknown"} inputJson={JSON.stringify(part.input)} />;
    if (part.type === "tool_result") {
      const raw = typeof part.content === "string" ? part.content : JSON.stringify(part.content);
      return (
        <div key={i} className="flex flex-col gap-1">
          <span className="font-mono text-[10px] text-purple-400/70">result · {part.tool_use_id}</span>
          <PlainText text={raw ?? ""} />
        </div>
      );
    }
    return null;
  });
}

function SummaryTab({
  reqBody,
  stream,
  prevBody,
}: {
  reqBody: AnthropicReqBody | null;
  stream: StreamResult;
  prevBody?: AnthropicReqBody | null;
}) {
  const sys = useMemo(() => resolveSystem(reqBody?.system), [reqBody]);

  const sharedIndices = useMemo(() => {
    const current = reqBody?.messages ?? [];
    const prev = prevBody?.messages ?? [];
    const shared = new Set<number>();
    for (let i = 0; i < current.length && i < prev.length; i++) {
      if (JSON.stringify(normalizeMsg(current[i])) === JSON.stringify(normalizeMsg(prev[i]))) {
        shared.add(i);
      }
    }
    return shared;
  }, [reqBody?.messages, prevBody?.messages]);

  return (
    <div className="flex flex-col gap-3 px-3 pb-3 overflow-y-auto flex-1 min-h-0 scrollbar-thin">
      {sys && (
        <Bubble role="system" defaultCollapsed>
          <PlainText text={sys} />
        </Bubble>
      )}

      {reqBody?.messages?.map((msg, i) => (
        <Bubble key={i} role={msg.role} defaultCollapsed={sharedIndices.has(i)} repeated={sharedIndices.has(i)}>
          {renderContent(msg.content)}
        </Bubble>
      ))}

      {stream.blocks.length > 0 && (
        <Bubble role="assistant">
          {stream.blocks.map((blk, i) =>
            blk.type === "text"
              ? <PlainText key={i} text={blk.text ?? ""} />
              : <ToolUseBlock key={i} name={blk.name ?? "unknown"} inputJson={blk.inputJson} />,
          )}
        </Bubble>
      )}
    </div>
  );
}

// ─── main detail panel ───────────────────────────────────────────────────────

export function RowDetailPanel({ row, prevRow, onPrev, onNext, applyConfig }: { row: TimelineRow; prevRow?: TimelineRow | null; onPrev?: () => void; onNext?: () => void; applyConfig?: (updater: ConfigUpdater) => Promise<void> }) {
  const req = row.rawEvents[0];
  const res = row.rawEvents.find(
    (e): e is Extract<SandboxEvent, { type: "egress.response" | "fs.response" }> =>
      e.type === "egress.response" || e.type === "fs.response",
  );
  const chunks = row.rawEvents.filter(
    (e): e is Extract<SandboxEvent, { type: "egress.stream_chunk" }> =>
      e.type === "egress.stream_chunk",
  );

  const isAnthropicMsg =
    req.type === "egress.request" &&
    req.host === "api.anthropic.com" &&
    req.path === "/v1/messages";

  const [tab, setTab] = useState<DetailTab>(isAnthropicMsg ? "summary" : "request");

  const anthropicReqBody = useMemo(
    () => (req.type === "egress.request" ? parseAnthropicRequest(req.body) : null),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [row],
  );
  const anthropicStream = useMemo(
    () => parseAnthropicStream(chunks),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [row],
  );
  const prevAnthropicReqBody = useMemo(() => {
    if (!prevRow) return null;
    const e = prevRow.rawEvents[0];
    const body = e.type === "egress.request" ? parseAnthropicRequest(e.body) : null;
    if (!body) return null;
    const prevChunks = prevRow.rawEvents.filter(
      (ev): ev is Extract<SandboxEvent, { type: "egress.stream_chunk" }> =>
        ev.type === "egress.stream_chunk",
    );
    const prevStream = parseAnthropicStream(prevChunks);
    if (prevStream.blocks.length === 0) return body;
    const assistantContent: MsgContentPart[] = prevStream.blocks.map((blk) =>
      blk.type === "text"
        ? { type: "text", text: blk.text ?? "" }
        : { type: "tool_use", id: blk.id ?? "", name: blk.name ?? "", input: (() => { try { return JSON.parse(blk.inputJson ?? "{}"); } catch { return {}; } })() },
    );
    return { ...body, messages: [...(body.messages ?? []), { role: "assistant", content: assistantContent }] };
  }, [prevRow]);

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

  const reqRawBody = req.type === "egress.request" ? req.body : undefined;
  const resRawBody = res?.type === "egress.response" ? res.body : undefined;

  const tabOptions: { value: DetailTab; label: string }[] = [
    ...(isAnthropicMsg ? [{ value: "summary" as DetailTab, label: "Summary" }] : []),
    { value: "request", label: "Request" },
    { value: "response", label: "Response" },
  ];

  return (
    <div className="flex flex-col h-full text-xs">
      <div className="flex shrink-0 items-center gap-2 px-3 py-2">
        <SegmentedControl options={tabOptions} value={tab} onChange={setTab} />
        <div className="ml-auto flex items-center gap-2">
          {isAnthropicMsg && tab === "summary" && (
            <>
              {(anthropicStream.model ?? anthropicReqBody?.model) && (
                <span className="font-mono text-[10px] bg-muted/50 rounded px-1.5 py-0.5 text-muted-foreground text-nowrap">
                  {anthropicStream.model ?? anthropicReqBody?.model}
                </span>
              )}
              {anthropicStream.stopReason && (
                <span className="font-mono text-[10px] bg-muted/50 rounded px-1.5 py-0.5 text-muted-foreground text-nowrap">
                  {anthropicStream.stopReason}
                </span>
              )}
              {(anthropicStream.inputTokens != null || anthropicStream.outputTokens != null) && (
                <span className="font-mono text-[10px] text-muted-foreground/60 text-nowrap">
                  {anthropicStream.inputTokens ?? "?"}↑ {anthropicStream.outputTokens ?? "?"}↓
                </span>
              )}
              {row.pending && !anthropicStream.stopReason && (
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

      {tab === "summary" && isAnthropicMsg && (
        <SummaryTab reqBody={anthropicReqBody} stream={anthropicStream} prevBody={prevAnthropicReqBody} />
      )}

      {tab === "request" && (
        <div className="flex flex-1 min-h-0 gap-3 px-3 pb-3">
          <div className={`rounded-md border border-border bg-muted/20 p-3 ${reqRawBody ? "shrink-0" : "flex-1"}`}>
            <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5">
              <KV label="time" value={ts} />
              {req.type === "egress.request" && <>
                <KV label="method" value={req.method} cls={egressMethodCls(req.method)} />
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
              </>}
              {req.type === "fs.request" && <>
                <KV label="op"     value={req.operation} />
                <KV label="path"   value={req.path} />
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
          {reqRawBody && <BodyBlock raw={reqRawBody} className="flex-1 min-w-0" />}
        </div>
      )}

      {tab === "response" && (
        <div className="flex flex-1 min-h-0 gap-3 px-3 pb-3">
          <div className={`flex flex-col gap-2 ${resRawBody || chunks.length > 0 ? "shrink-0" : "flex-1"}`}>
            {res ? (
              <div className="rounded-md border border-border bg-muted/20 p-3">
                <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-0.5">
                  {res.type === "egress.response" && <>
                    <KV label="status"   value={String(res.status)} cls={res.status >= 400 ? "text-red-400" : "text-green-400"} />
                    <KV label="duration" value={`${res.duration_ms}ms`} />
                  </>}
                  {res.type === "fs.response" && <>
                    <KV label="backend"  value={res.backend} />
                    <KV label="duration" value={`${res.duration_ms}ms`} />
                    {res.error && <KV label="error" value={res.error} cls="text-red-400" />}
                  </>}
                </div>
              </div>
            ) : (
              <div className="rounded-md border border-border/40 bg-muted/10 p-3 flex items-center justify-center">
                <span className="text-muted-foreground">{row.pending ? "awaiting response…" : "no response received"}</span>
              </div>
            )}
          </div>
          {(resRawBody || chunks.length > 0) && (
            <div className="flex flex-1 min-h-0 min-w-0 flex-col gap-1 overflow-hidden">
              {resRawBody && <BodyBlock raw={resRawBody} className="shrink-0" />}
              {chunks.length > 0 && (
                <div className="flex flex-1 min-h-0 flex-col rounded-md border border-border bg-muted/20 overflow-hidden">
                  <CodeViewer content={chunks.map((c) => c.body).join("\n\n")} lang="text" className="flex-1 min-h-0" />
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
