import type {
  EgressChunk,
  LLMContentBlock,
  LLMMessage,
  LLMProvider,
  LLMSummaryData,
} from "./llmProviders";

interface GeminiPart {
  text?: string;
  functionCall?: { name?: string; args?: unknown };
  functionResponse?: { name?: string; response?: unknown };
}

interface GeminiContent {
  role?: string;
  parts?: GeminiPart[];
}

interface GeminiReqBody {
  model?: string;
  contents?: GeminiContent[];
  systemInstruction?: { parts?: GeminiPart[] };
}

interface GeminiResponseChunk {
  candidates?: Array<{
    content?: GeminiContent;
    finishReason?: string;
  }>;
  usageMetadata?: {
    promptTokenCount?: number;
    candidatesTokenCount?: number;
  };
  modelVersion?: string;
}

function geminiRoleToLLM(role?: string): string {
  return role === "model" ? "assistant" : (role ?? "user");
}

function partToBlock(part: GeminiPart): LLMContentBlock | null {
  if (part.text !== undefined) return { type: "text", text: part.text };
  if (part.functionCall)
    return {
      type: "tool_use",
      toolName: part.functionCall.name,
      toolInput: part.functionCall.args,
    };
  if (part.functionResponse)
    return {
      type: "tool_result",
      toolId: part.functionResponse.name,
      toolResultContent: part.functionResponse.response,
    };
  return null;
}

function parseChunks(chunks: EgressChunk[]): {
  blocks: LLMContentBlock[];
  stopReason?: string;
  inputTokens?: number;
  outputTokens?: number;
  model?: string;
} {
  const blocks: LLMContentBlock[] = [];
  let stopReason: string | undefined;
  let inputTokens: number | undefined;
  let outputTokens: number | undefined;
  let model: string | undefined;

  for (const chunk of chunks) {
    // streamGenerateContent returns either SSE ("data: {...}") or raw JSON lines
    for (const line of chunk.body.split("\n")) {
      const trimmed = line.trimEnd();
      const jsonStr = trimmed.startsWith("data: ") ? trimmed.slice(6) : trimmed;
      if (!jsonStr || jsonStr === "[DONE]") continue;
      let msg: GeminiResponseChunk;
      try {
        msg = JSON.parse(jsonStr) as GeminiResponseChunk;
      } catch {
        continue;
      }

      if (msg.modelVersion) model = msg.modelVersion;

      const candidate = msg.candidates?.[0];
      if (candidate) {
        if (candidate.finishReason) stopReason = candidate.finishReason;
        for (const part of candidate.content?.parts ?? []) {
          const blk = partToBlock(part);
          if (blk) {
            // Accumulate text into the last text block rather than emitting many small ones
            if (
              blk.type === "text" &&
              blocks.length > 0 &&
              blocks[blocks.length - 1].type === "text"
            ) {
              blocks[blocks.length - 1].text =
                (blocks[blocks.length - 1].text ?? "") + (blk.text ?? "");
            } else {
              blocks.push(blk);
            }
          }
        }
      }

      if (msg.usageMetadata) {
        inputTokens = msg.usageMetadata.promptTokenCount;
        outputTokens = msg.usageMetadata.candidatesTokenCount;
      }
    }
  }

  return { blocks, stopReason, inputTokens, outputTokens, model };
}

export const geminiProvider: LLMProvider = {
  matches(event) {
    return (
      event.host === "cloudcode-pa.googleapis.com" &&
      (event.path.endsWith(":streamGenerateContent") ||
        event.path.endsWith(":generateContent"))
    );
  },

  extractLabel(event) {
    if (!this.matches(event)) return null;
    // Model may appear in the path: /v1/models/{model}:streamGenerateContent
    const pathModel = event.path.match(/\/models\/([^/:]+):/)?.[1];
    if (pathModel) return pathModel;
    if (!event.body) return null;
    try {
      return (JSON.parse(event.body) as GeminiReqBody).model ?? null;
    } catch {
      return null;
    }
  },

  parseSummary(request, _response, chunks) {
    if (!this.matches(request)) return null;

    let reqBody: GeminiReqBody | null = null;
    try {
      reqBody = request.body
        ? (JSON.parse(request.body) as GeminiReqBody)
        : null;
    } catch {
      /* ignore */
    }

    const pathModel = request.path.match(/\/models\/([^/:]+):/)?.[1];
    const stream = parseChunks(chunks);

    const systemParts = reqBody?.systemInstruction?.parts ?? [];
    const system =
      systemParts
        .map((p) => p.text ?? "")
        .filter(Boolean)
        .join("\n") || undefined;

    const messages: LLMMessage[] = (reqBody?.contents ?? []).map((c) => ({
      role: geminiRoleToLLM(c.role),
      content: (c.parts ?? [])
        .map(partToBlock)
        .filter((b): b is LLMContentBlock => b !== null),
    }));

    return {
      model: stream.model ?? pathModel ?? reqBody?.model,
      system,
      messages,
      response:
        stream.blocks.length > 0 || stream.stopReason
          ? { blocks: stream.blocks, stopReason: stream.stopReason }
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
