import type { SandboxEvent } from "@/types";
import type {
  LLMProvider,
  LLMContentBlock,
  LLMMessage,
  LLMSummaryData,
} from "./llmProviders";

type EgressChunk = Extract<SandboxEvent, { type: "egress.chunk" }>;

interface ChatGPTReqBody {
  model?: string;
  instructions?: string;
  input?: Array<{
    type: string;
    role?: string;
    content?: string | Array<{ type: string; text?: string }>;
  }>;
}

interface OutputItem {
  id: string;
  type: "message" | "function_call";
  name?: string;
  text: string;
}

interface StreamResult {
  model?: string;
  items: OutputItem[];
  inputTokens?: number;
  outputTokens?: number;
  stopReason?: string;
}

function parseStream(chunks: EgressChunk[]): StreamResult {
  const items: Record<string, OutputItem> = {};
  let model: string | undefined;
  let inputTokens: number | undefined;
  let outputTokens: number | undefined;
  let stopReason: string | undefined;

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
        case "response.created": {
          const r = msg.response as Record<string, unknown> | undefined;
          if (r?.model) model = r.model as string;
          break;
        }
        case "response.output_item.added": {
          const item = msg.item as Record<string, unknown> | undefined;
          if (!item?.id) break;
          items[item.id as string] = {
            id: item.id as string,
            type: (item.type as "message" | "function_call") ?? "message",
            name: item.name as string | undefined,
            text: "",
          };
          break;
        }
        case "response.output_text.delta": {
          const itemId = msg.item_id as string | undefined;
          const delta = msg.delta as string | undefined;
          if (itemId && delta && items[itemId]) items[itemId].text += delta;
          break;
        }
        case "response.function_call_arguments.delta": {
          const itemId = msg.item_id as string | undefined;
          const delta = msg.delta as string | undefined;
          if (itemId && delta && items[itemId]) items[itemId].text += delta;
          break;
        }
        case "response.completed": {
          const r = msg.response as Record<string, unknown> | undefined;
          if (r?.model) model = r.model as string;
          const usage = r?.usage as Record<string, number> | undefined;
          if (usage) {
            inputTokens = usage.input_tokens;
            outputTokens = usage.output_tokens;
          }
          stopReason = (r?.incomplete_details as
            | Record<string, unknown>
            | undefined)
            ? "incomplete"
            : "stop";
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

function normalizeInputContent(
  content: string | Array<{ type: string; text?: string }> | undefined,
): LLMContentBlock[] {
  if (!content) return [];
  if (typeof content === "string")
    return content ? [{ type: "text", text: content }] : [];
  return content
    .filter((p) => p.type === "input_text" || p.type === "text")
    .map((p) => ({ type: "text" as const, text: p.text }));
}

export const chatgptProvider: LLMProvider = {
  matches(event) {
    return (
      event.host === "chatgpt.com" &&
      event.path === "/backend-api/codex/responses"
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

    const messages: LLMMessage[] = (reqBody?.input ?? [])
      .filter((item) => item.type === "message" && item.role)
      .map((item) => ({
        role: item.role!,
        content: normalizeInputContent(item.content),
      }));

    const responseBlocks: LLMContentBlock[] = stream.items.map((item) => {
      if (item.type === "function_call") {
        let toolInput: unknown;
        try {
          toolInput = JSON.parse(item.text || "{}");
        } catch {
          toolInput = {};
        }
        return { type: "tool_use" as const, toolName: item.name, toolInput };
      }
      return { type: "text" as const, text: item.text };
    });

    return {
      model: stream.model ?? reqBody?.model,
      system: reqBody?.instructions,
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
