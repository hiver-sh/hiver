import type { SandboxEvent } from "@/types";
import type {
  LLMProvider,
  LLMContentBlock,
  LLMMessage,
  LLMSummaryData,
  LLMTool,
} from "./llmProviders";

type EgressChunk = Extract<SandboxEvent, { type: "egress.chunk" }>;

// Codex's Responses API payload. Tools and the system prompt are no longer
// top-level fields: tool definitions arrive as an `additional_tools` input
// item, and the system prompt as `developer`-role message items. Assistant
// history uses `output_text` content and `custom_tool_call`/`function_call`
// items for tool use + their `*_output` counterparts for results.
interface ChatGPTTool {
  type: string;
  name?: string;
  description?: string;
  parameters?: unknown;
  tools?: ChatGPTTool[];
}

interface ContentPart {
  type: string;
  text?: string;
}

interface InputItem {
  type: string;
  role?: string;
  content?: string | ContentPart[];
  tools?: ChatGPTTool[];
  name?: string;
  call_id?: string;
  input?: string;
  arguments?: string;
  output?: unknown;
}

interface ChatGPTReqBody {
  model?: string;
  instructions?: string;
  input?: InputItem[];
}

interface OutputItem {
  id: string;
  // "message" is assistant text; anything else is a tool call whose accumulated
  // `text` is the (JSON/JS) arguments.
  type: string;
  name?: string;
  // Stable tool-call id (matches the later `*_output` item's `call_id`). Used
  // as the block's toolId so the timeline can correlate use → result.
  callId?: string;
  text: string;
}

interface StreamResult {
  model?: string;
  items: OutputItem[];
  inputTokens?: number;
  outputTokens?: number;
  stopReason?: string;
}

function isToolCall(type: string): boolean {
  return type === "function_call" || type === "custom_tool_call";
}

function parseStream(chunks: EgressChunk[]): StreamResult {
  const items: Record<string, OutputItem> = {};
  let model: string | undefined;
  let inputTokens: number | undefined;
  let outputTokens: number | undefined;
  let stopReason: string | undefined;

  const upsert = (
    id: string,
    type?: string,
    name?: string,
    callId?: string,
  ): OutputItem => {
    let item = items[id];
    if (!item) {
      item = { id, type: type ?? "message", text: "" };
      items[id] = item;
    }
    if (type) item.type = type;
    if (name) item.name = name;
    if (callId) item.callId = callId;
    return item;
  };

  for (const chunk of chunks) {
    for (const line of chunk.body.split("\n")) {
      const trimmed = line.trimEnd();
      if (!trimmed.startsWith("data: ")) continue;
      const data = trimmed.slice(6);
      if (!data || data === "[DONE]") continue;
      let msg: Record<string, unknown>;
      try {
        msg = JSON.parse(data) as Record<string, unknown>;
      } catch {
        continue;
      }

      switch (msg.type) {
        case "response.created":
        case "response.in_progress":
        case "response.completed": {
          const r = msg.response as Record<string, unknown> | undefined;
          if (r?.model) model = r.model as string;
          const usage = r?.usage as Record<string, number> | undefined;
          if (usage) {
            inputTokens = usage.input_tokens;
            outputTokens = usage.output_tokens;
          }
          if (msg.type === "response.completed") {
            stopReason = (r?.incomplete_details as
              | Record<string, unknown>
              | undefined)
              ? "incomplete"
              : "stop";
          }
          break;
        }
        case "response.output_item.added": {
          const item = msg.item as Record<string, unknown> | undefined;
          if (item?.id)
            upsert(
              item.id as string,
              item.type as string | undefined,
              item.name as string | undefined,
              item.call_id as string | undefined,
            );
          break;
        }
        case "response.output_text.delta":
        case "response.function_call_arguments.delta":
        case "response.custom_tool_call_input.delta": {
          const itemId = msg.item_id as string | undefined;
          const delta = msg.delta as string | undefined;
          if (itemId && delta) upsert(itemId).text += delta;
          break;
        }
        case "response.output_item.done": {
          // Authoritative final form of the item — prefer it over accumulated
          // deltas (which can be missing if the stream was joined mid-flight).
          const item = msg.item as Record<string, unknown> | undefined;
          if (!item?.id) break;
          const stored = upsert(
            item.id as string,
            item.type as string | undefined,
            item.name as string | undefined,
            item.call_id as string | undefined,
          );
          const finalArgs =
            (item.input as string | undefined) ??
            (item.arguments as string | undefined);
          if (isToolCall(stored.type) && finalArgs != null)
            stored.text = finalArgs;
          break;
        }
      }
    }
  }

  return {
    model,
    items: Object.values(items),
    inputTokens,
    outputTokens,
    stopReason,
  };
}

function normalizeContent(
  content: string | ContentPart[] | undefined,
): LLMContentBlock[] {
  if (!content) return [];
  if (typeof content === "string")
    return content ? [{ type: "text", text: content }] : [];
  return content
    .filter(
      (p) =>
        p.type === "input_text" ||
        p.type === "output_text" ||
        p.type === "text",
    )
    .map((p) => ({ type: "text" as const, text: p.text }));
}

function normalizeOutput(output: unknown): string {
  if (typeof output === "string") return output;
  if (Array.isArray(output))
    return output
      .map((p) =>
        p && typeof p === "object" && "text" in p
          ? String((p as ContentPart).text ?? "")
          : typeof p === "string"
            ? p
            : JSON.stringify(p),
      )
      .join("");
  return JSON.stringify(output);
}

// Flatten tool definitions, expanding `namespace` groups (e.g. `collaboration`)
// into their nested tools with a dotted name.
function flattenTools(tools: ChatGPTTool[] | undefined, prefix = ""): LLMTool[] {
  if (!tools) return [];
  const out: LLMTool[] = [];
  for (const t of tools) {
    if (t.type === "namespace" && t.tools) {
      out.push(...flattenTools(t.tools, `${prefix}${t.name ?? ""}.`));
      continue;
    }
    if (!t.name) continue;
    out.push({
      name: `${prefix}${t.name}`,
      description: t.description,
      inputSchema: t.parameters,
    });
  }
  return out;
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text || "{}");
  } catch {
    // Codex `exec` custom tools stream raw JavaScript, not JSON.
    return text;
  }
}

export const chatgptProvider: LLMProvider = {
  matches(event) {
    // Codex picks its endpoint by auth mode: chatgpt.com/backend-api/codex for
    // ChatGPT-login sessions, api.openai.com/v1 for API-key auth. Both speak the
    // same Responses-API payload, so handle either here.
    return (
      (event.host === "chatgpt.com" &&
        event.path === "/backend-api/codex/responses") ||
      (event.host === "api.openai.com" && event.path === "/v1/responses")
    );
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

    let reqBody: ChatGPTReqBody | null = null;
    try {
      reqBody = request.body
        ? (JSON.parse(request.body) as ChatGPTReqBody)
        : null;
    } catch {
      /* ignore */
    }

    const stream = parseStream(chunks);

    const systemParts: string[] = [];
    const tools: LLMTool[] = [];
    const messages: LLMMessage[] = [];

    for (const item of reqBody?.input ?? []) {
      switch (item.type) {
        case "additional_tools":
          tools.push(...flattenTools(item.tools));
          break;
        case "message": {
          // `developer` role carries the system prompt (Codex has no top-level
          // `instructions` field); fold it into the system bubble.
          if (item.role === "developer") {
            for (const blk of normalizeContent(item.content))
              if (blk.text) systemParts.push(blk.text);
            break;
          }
          if (!item.role) break;
          messages.push({
            role: item.role,
            content: normalizeContent(item.content),
          });
          break;
        }
        case "function_call":
        case "custom_tool_call":
          messages.push({
            role: "assistant",
            content: [
              {
                type: "tool_use",
                toolName: item.name,
                toolId: item.call_id,
                toolInput: safeParse(item.input ?? item.arguments ?? ""),
              },
            ],
          });
          break;
        case "function_call_output":
        case "custom_tool_call_output":
          messages.push({
            role: "tool",
            content: [
              {
                type: "tool_result",
                toolId: item.call_id,
                toolResultContent: normalizeOutput(item.output),
              },
            ],
          });
          break;
      }
    }

    // `instructions` may still be present on older payloads; prefer it.
    const system =
      reqBody?.instructions ??
      (systemParts.length > 0 ? systemParts.join("\n\n") : undefined);

    const responseBlocks: LLMContentBlock[] = stream.items.map((item) => {
      if (isToolCall(item.type)) {
        return {
          type: "tool_use" as const,
          toolName: item.name,
          toolId: item.callId,
          toolInput: safeParse(item.text),
        };
      }
      return { type: "text" as const, text: item.text };
    });

    return {
      model: stream.model ?? reqBody?.model,
      system,
      tools: tools.length > 0 ? tools : undefined,
      messages,
      response:
        responseBlocks.length > 0 || stream.stopReason
          ? { blocks: responseBlocks, stopReason: stream.stopReason }
          : undefined,
      usage:
        stream.inputTokens != null || stream.outputTokens != null
          ? {
              inputTokens: stream.inputTokens,
              outputTokens: stream.outputTokens,
            }
          : undefined,
    } satisfies LLMSummaryData;
  },
};
