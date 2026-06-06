import { useMemo, useState } from "react";
import { ChevronDown, ChevronRight, History } from "lucide-react";
import type { ReactNode } from "react";
import { CodeViewer } from "./CodeViewer";
import type { LLMSummaryData, LLMContentBlock, LLMTool } from "@/lib/llmProviders";

function tryPretty(body?: string): { content: string; isJson: boolean } | undefined {
  if (!body) return undefined;
  try { return { content: JSON.stringify(JSON.parse(body), null, 2), isJson: true }; }
  catch { return { content: body, isJson: false }; }
}

function prettyJson(s?: string): string {
  if (!s) return "";
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

function Bubble({ role, children, defaultCollapsed = false }: { role: string; children: ReactNode; defaultCollapsed?: boolean }) {
  const [collapsed, setCollapsed] = useState(defaultCollapsed);
  return (
    <div className="flex flex-col rounded-lg border border-border bg-card">
      <button
        className={`flex items-center gap-1.5 px-3 py-2 text-[10px] uppercase font-semibold tracking-widest text-muted-foreground hover:bg-muted/40 transition-colors text-left w-full${!collapsed ? " border-b border-border/60" : ""}`}
        onClick={() => setCollapsed((v) => !v)}
      >
        {collapsed ? <ChevronRight className="h-3 w-3 shrink-0" /> : <ChevronDown className="h-3 w-3 shrink-0" />}
        {role}
      </button>
      {!collapsed && (
        <div className="px-3 py-3 flex flex-col gap-3">
          {children}
        </div>
      )}
    </div>
  );
}

function PlainText({ text }: { text: string }) {
  return (
    <p className="text-sm leading-relaxed break-words text-foreground select-text whitespace-pre-wrap">{text}</p>
  );
}

function ToolUseBlock({ name, input }: { name: string; input?: unknown }) {
  const pretty = useMemo(() => prettyJson(input !== undefined ? JSON.stringify(input) : undefined), [input]);
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-[11px] font-medium text-muted-foreground tracking-wide">Tool called: <span className="text-foreground/80">{name}</span></span>
      {pretty && (
        <div className="rounded-md border border-border overflow-hidden">
          <CodeViewer content={pretty} lang="json" minHeight={60} maxHeight={160} />
        </div>
      )}
    </div>
  );
}

function renderBlocks(blocks: LLMContentBlock[], toolIdToName?: Map<string, string>): ReactNode {
  return blocks.map((blk, i) => {
    if (blk.type === "text" && blk.text)
      return <PlainText key={i} text={blk.text} />;
    if (blk.type === "tool_use")
      return <ToolUseBlock key={i} name={blk.toolName ?? "unknown"} input={blk.toolInput} />;
    if (blk.type === "tool_result") {
      const raw = typeof blk.toolResultContent === "string" ? blk.toolResultContent : JSON.stringify(blk.toolResultContent);
      const { content: pretty, isJson } = tryPretty(raw) ?? { content: raw ?? "", isJson: false };
      const resolvedName = (blk.toolId && toolIdToName?.get(blk.toolId)) ?? blk.toolId ?? "unknown";
      return (
        <div key={i} className="flex flex-col gap-1.5">
          <span className="text-[11px] font-medium text-muted-foreground tracking-wide">Tool result: <span className="text-foreground/80">{resolvedName}</span></span>
          <div className="rounded-md border border-border overflow-hidden">
            <CodeViewer content={pretty} lang={isJson ? "json" : "text"} minHeight={60} maxHeight={320} />
          </div>
        </div>
      );
    }
    return null;
  });
}

function DefinedToolsBlock({ tools }: { tools: LLMTool[] }) {
  return (
    <Bubble role={`tools (${tools.length})`} defaultCollapsed>
      <div className="flex flex-col divide-y divide-border">
        {tools.map((tool, i) => (
          <div key={i} className="flex flex-col gap-1 py-2 first:pt-0 last:pb-0">
            <span className="text-xs font-medium text-foreground/90 select-text">{tool.name}</span>
            {tool.description && (
              <span className="text-[11px] text-muted-foreground select-text leading-relaxed whitespace-pre-wrap">{tool.description}</span>
            )}
          </div>
        ))}
      </div>
    </Bubble>
  );
}

export function LLMSummary({ summary, prevSummary }: { summary: LLMSummaryData; prevSummary?: LLMSummaryData | null }) {
  const sharedIndices = useMemo(() => {
    const current = summary.messages;
    const prev = prevSummary?.messages ?? [];
    const shared = new Set<number>();
    for (let i = 0; i < current.length && i < prev.length; i++) {
      if (JSON.stringify(current[i]) === JSON.stringify(prev[i])) shared.add(i);
    }
    return shared;
  }, [summary.messages, prevSummary?.messages]);

  const toolIdToName = useMemo(() => {
    const map = new Map<string, string>();
    const allBlocks = [
      ...(summary.response?.blocks ?? []),
      ...summary.messages.flatMap((m) => m.content),
    ];
    for (const blk of allBlocks) {
      if (blk.type === "tool_use" && blk.toolId && blk.toolName) {
        map.set(blk.toolId, blk.toolName);
      }
    }
    return map;
  }, [summary]);

  const [prevContextExpanded, setPrevContextExpanded] = useState(false);

  const indexed = useMemo(
    () => summary.messages.map((msg, i) => ({ msg, i })),
    [summary.messages],
  );
  const currentMsgs = indexed.filter(({ i }) => !sharedIndices.has(i));
  const prevMsgs = indexed.filter(({ i }) => sharedIndices.has(i));

  return (
    <div className="flex flex-1 min-h-0 flex-col gap-3 overflow-y-auto px-3 pb-4 pt-1 scroll-container">
      {summary.system && (
        <Bubble role="system" defaultCollapsed>
          <PlainText text={summary.system} />
        </Bubble>
      )}

      {summary.tools && summary.tools.length > 0 && (
        <DefinedToolsBlock tools={summary.tools} />
      )}

      {prevMsgs.length > 0 && (
        <div className="flex flex-col gap-3">
          <button
            type="button"
            onClick={() => setPrevContextExpanded((v) => !v)}
            className="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-muted-foreground/50 font-semibold hover:text-muted-foreground/80 transition-colors w-fit"
          >
            {prevContextExpanded ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
            <History className="h-3 w-3" />
            Previous context ({prevMsgs.length} {prevMsgs.length === 1 ? "message" : "messages"})
          </button>
          {prevContextExpanded && prevMsgs.map(({ msg, i }) => (
            <Bubble key={i} role={msg.role}>
              {renderBlocks(msg.content, toolIdToName)}
            </Bubble>
          ))}
        </div>
      )}

      {currentMsgs.map(({ msg, i }) => (
        <Bubble key={i} role={msg.role}>
          {renderBlocks(msg.content, toolIdToName)}
        </Bubble>
      ))}

      {summary.response && summary.response.blocks.length > 0 && (
        <Bubble role="assistant">
          {renderBlocks(summary.response.blocks, toolIdToName)}
        </Bubble>
      )}
    </div>
  );
}
