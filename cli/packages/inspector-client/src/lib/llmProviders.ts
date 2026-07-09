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

export const LLM_PROVIDERS: LLMProvider[] = [
  anthropicProvider,
  chatgptProvider,
  geminiProvider,
];

// ─── Per-event memoization ────────────────────────────────────────────────
// `buildRows` re-runs over the entire accumulated event feed on every frame a
// batch of events is appended, and for each LLM egress event it calls
// extractLabel/parseSummary — both of which JSON.parse the (large, growing)
// request body and re-parse every response chunk. Re-parsing every past event
// on every frame makes the timeline cost grow quadratically with the session
// length: that's the ~500ms render passes seen in profiles of long sessions.
//
// Events are immutable and referentially stable (the feed is append-only), so
// results can be cached on the request event object. extractLabel depends only
// on the (immutable) request body, so it caches unconditionally. parseSummary
// also folds in the response + streamed chunks, so its cache is invalidated
// while those are still growing (keyed on chunk count + response presence) and
// only becomes a stable hit once the request completes.

const labelCache = new WeakMap<EgressRequest, string | null>();

export function extractLabelCached(
  provider: LLMProvider,
  event: EgressRequest,
): string | null {
  const hit = labelCache.get(event);
  if (hit !== undefined || labelCache.has(event)) return hit ?? null;
  const value = provider.extractLabel(event);
  labelCache.set(event, value);
  return value;
}

interface SummaryCacheEntry {
  chunkLen: number;
  hasResponse: boolean;
  value: LLMSummaryData | null;
}
const summaryCache = new WeakMap<EgressRequest, SummaryCacheEntry>();

export function parseSummaryCached(
  provider: LLMProvider,
  request: EgressRequest,
  response: EgressResponse | undefined,
  chunks: EgressChunk[],
): LLMSummaryData | null {
  const hasResponse = response !== undefined;
  const cached = summaryCache.get(request);
  if (
    cached &&
    cached.chunkLen === chunks.length &&
    cached.hasResponse === hasResponse
  ) {
    return cached.value;
  }
  const value = provider.parseSummary(request, response, chunks);
  summaryCache.set(request, { chunkLen: chunks.length, hasResponse, value });
  return value;
}

// Which provider (if any) owns an egress request. `matches()` is a cheap
// host/path check, but caching the resolved provider keeps the per-event
// lookup in `buildRows` from scanning the provider list on every rebuild.
const providerCache = new WeakMap<EgressRequest, LLMProvider | null>();

export function matchProvider(event: EgressRequest): LLMProvider | null {
  const cached = providerCache.get(event);
  if (cached !== undefined) return cached;
  const provider = LLM_PROVIDERS.find((p) => p.matches(event)) ?? null;
  providerCache.set(event, provider);
  return provider;
}
