import type { SandboxEvent } from "@/types";
import type { LLMProvider, LLMContentBlock, LLMMessage, LLMSummaryData, LLMTool } from "./llmProviders";

type EgressChunk = Extract<SandboxEvent, { type: "egress.chunk" }>;

interface AnthropicStreamBlock {
  type: "text" | "tool_use";
  text?: string;
  id?: string;
  name?: string;
  inputJson?: string;
}

interface AnthropicStreamResult {
  blocks: AnthropicStreamBlock[];
  stopReason?: string;
  inputTokens?: number;
  outputTokens?: number;
  model?: string;
}

type MsgContentPart =
  | { type: "text"; text?: string }
  | { type: "tool_use"; id?: string; name?: string; input?: unknown }
  | { type: "tool_result"; tool_use_id?: string; content?: unknown }
  | { type: string; [k: string]: unknown };

interface AnthropicReqBody {
  model?: string;
  system?: string | Array<{ type: string; text?: string }>;
  tools?: Array<{ name?: string; description?: string; input_schema?: unknown }>;
  messages?: Array<{ role: string; content: string | MsgContentPart[] }>;
}

interface AnthropicResBody {
  content?: MsgContentPart[];
  stop_reason?: string;
  model?: string;
  usage?: { input_tokens?: number; output_tokens?: number };
}

function isThinkingBlock(v: unknown): boolean {
  if (!v || typeof v !== "object") return false;
  const t = (v as Record<string, unknown>).type;
  return t === "thinking" || t === "redacted_thinking";
}

function resolveSystem(system?: string | Array<{ type: string; text?: string }>): string | undefined {
  if (!system) return undefined;
  if (typeof system === "string") return system || undefined;
  return system.filter((b) => b.type === "text").map((b) => b.text ?? "").join("\n") || undefined;
}

function msgPartToBlock(part: MsgContentPart): LLMContentBlock | null {
  if (isThinkingBlock(part)) return null;
  if (part.type === "text") return { type: "text", text: (part as { text?: string }).text };
  if (part.type === "tool_use") {
    const p = part as { id?: string; name?: string; input?: unknown };
    return { type: "tool_use", toolId: p.id, toolName: p.name, toolInput: p.input };
  }
  if (part.type === "tool_result") {
    const p = part as { tool_use_id?: string; content?: unknown };
    return { type: "tool_result", toolId: p.tool_use_id, toolResultContent: p.content };
  }
  return null;
}

function normalizeContent(content: string | MsgContentPart[]): LLMContentBlock[] {
  if (typeof content === "string") return content ? [{ type: "text", text: content }] : [];
  return content.map(msgPartToBlock).filter((b): b is LLMContentBlock => b !== null);
}

function parseStream(chunks: EgressChunk[]): AnthropicStreamResult {
  const blocks: AnthropicStreamBlock[] = [];
  let stopReason: string | undefined;
  let inputTokens: number | undefined;
  let outputTokens: number | undefined;
  let model: string | undefined;

  for (const chunk of chunks) {
    for (const line of chunk.body.split("\n")) {
      const trimmed = line.trimEnd();
      if (!trimmed.startsWith("data: ")) continue;
      const data = trimmed.slice(6);
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
  }

  return { blocks: blocks.filter(Boolean), stopReason, inputTokens, outputTokens, model };
}

function streamBlockToContentBlock(blk: AnthropicStreamBlock): LLMContentBlock {
  if (blk.type === "text") return { type: "text", text: blk.text };
  let toolInput: unknown = undefined;
  try { toolInput = JSON.parse(blk.inputJson ?? "{}"); } catch { toolInput = {}; }
  return { type: "tool_use", toolId: blk.id, toolName: blk.name, toolInput };
}

export const anthropicProvider: LLMProvider = {
  matches(event) {
    return event.host === "api.anthropic.com" && event.path === "/v1/messages";
  },

  extractLabel(event) {
    if (!this.matches(event) || !event.body) return null;
    try {
      return (JSON.parse(event.body) as { model?: string }).model ?? null;
    } catch {
      return null;
    }
  },

  parseSummary(request, _response, chunks) {
    if (!this.matches(request)) return null;

    let reqBody: AnthropicReqBody | null = null;
    try { reqBody = request.body ? (JSON.parse(request.body) as AnthropicReqBody) : null; } catch { /* ignore */ }

    const stream = parseStream(chunks);

    // Non-streaming: chunks carry a plain JSON body with no SSE lines
    let resBody: AnthropicResBody | null = null;
    if (chunks.length > 0 && stream.blocks.length === 0) {
      try { resBody = JSON.parse(chunks.map((c) => c.body).join("")) as AnthropicResBody; } catch { /* ignore */ }
    }

    const messages: LLMMessage[] = (reqBody?.messages ?? []).map((msg) => ({
      role: msg.role,
      content: normalizeContent(msg.content),
    }));

    const responseBlocks: LLMContentBlock[] =
      stream.blocks.length > 0
        ? stream.blocks.map(streamBlockToContentBlock)
        : (resBody?.content ?? []).map(msgPartToBlock).filter((b): b is LLMContentBlock => b !== null);

    const stopReason = stream.stopReason ?? resBody?.stop_reason;
    const model = stream.model ?? resBody?.model ?? reqBody?.model;
    const inputTokens = stream.inputTokens ?? resBody?.usage?.input_tokens;
    const outputTokens = stream.outputTokens ?? resBody?.usage?.output_tokens;

    const tools: LLMTool[] | undefined = reqBody?.tools?.length
      ? reqBody.tools.map((t) => ({ name: t.name ?? "unknown", description: t.description, inputSchema: t.input_schema }))
      : undefined;

    return {
      model,
      system: resolveSystem(reqBody?.system),
      tools,
      messages,
      response: responseBlocks.length > 0 || stopReason
        ? { blocks: responseBlocks, stopReason }
        : undefined,
      usage: inputTokens != null || outputTokens != null ? { inputTokens, outputTokens } : undefined,
    } satisfies LLMSummaryData;
  },
};
