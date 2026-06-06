import type { SandboxEvent } from "@/types";
import { anthropicProvider } from "./anthropicProvider";
import { chatgptProvider } from "./chatgptProvider";
import { geminiProvider } from "./geminiProvider";

export type EgressRequest = Extract<SandboxEvent, { type: "egress.request" }>;
export type EgressResponse = Extract<SandboxEvent, { type: "egress.response" }>;
export type EgressChunk = Extract<SandboxEvent, { type: "egress.chunk" }>;

export interface LLMContentBlock {
  type: "text" | "tool_use" | "tool_result";
  text?: string;
  toolName?: string;
  toolId?: string;
  toolInput?: unknown;
  toolResultContent?: unknown;
}

export interface LLMMessage {
  role: string;
  content: LLMContentBlock[];
}

export interface LLMTool {
  name: string;
  description?: string;
  inputSchema?: unknown;
}

export interface LLMSummaryData {
  model?: string;
  system?: string;
  tools?: LLMTool[];
  messages: LLMMessage[];
  response?: {
    blocks: LLMContentBlock[];
    stopReason?: string;
  };
  usage?: { inputTokens?: number; outputTokens?: number };
}

export interface LLMProvider {
  /** True if this provider owns the request (host/path check, no body needed). */
  matches(event: EgressRequest): boolean;
  /** Return the model/label string if this provider owns the request, null otherwise. */
  extractLabel(event: EgressRequest): string | null;
  /** Parse request + response into a summary, or null if this provider doesn't own the request. */
  parseSummary(
    request: EgressRequest,
    response: EgressResponse | undefined,
    chunks: EgressChunk[],
  ): LLMSummaryData | null;
}

export const LLM_PROVIDERS: LLMProvider[] = [anthropicProvider, chatgptProvider, geminiProvider];
