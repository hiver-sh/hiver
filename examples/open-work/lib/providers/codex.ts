import type { Sandbox } from "@hiver.sh/client";

import { isSystemHint } from "../system-message";
import type { ConversationMessage } from "../types";
import { SYSTEM_PROMPT } from "./system-prompt";
import type { DriverContext, Provider, SessionDriver } from "./types";

// Codex (app-server) persists each thread as a rollout JSONL under
// ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<threadId>.jsonl, so the newest one
// is the task's conversation and its filename carries the id to resume.
const CODEX_SESSIONS_DIR = "/home/agent/.codex/sessions";
const CODEX_ROLLOUT_RE =
  /-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$/;

/**
 * Drives Codex over `app-server` JSON-RPC (jsonl in/out): an initialize →
 * thread handshake, then one `turn/start` per user turn, mapping the streamed
 * notifications to bus events.
 */
class CodexDriver implements SessionDriver {
  private threadId = "";
  private turnSeq = 0;
  private ready?: Promise<void>;
  private resolveReady?: () => void;
  private sawText = false; // any text emitted this turn (for paragraph breaks)
  private lastItemId = ""; // the agentMessage item currently streaming

  constructor(private ctx: DriverContext) {}

  start(): void {
    this.ready = new Promise((resolve) => (this.resolveReady = resolve));
    void this.writeJson({
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: { clientInfo: { name: "open-work", version: "0.1" } },
    });
  }

  async send(prompt: string): Promise<void> {
    await this.ready; // wait for the thread id from the handshake
    // A system hint continues the current turn (keep the separator context); a
    // real user turn starts fresh.
    if (!this.ctx.isSystemHint(prompt)) {
      this.sawText = false;
      this.lastItemId = "";
    }
    // ids 1 (initialize) and 2 (thread/start|resume) belong to the handshake.
    const id = 3 + this.turnSeq++;
    await this.writeJson({
      jsonrpc: "2.0",
      id,
      method: "turn/start",
      params: {
        threadId: this.threadId,
        input: [{ type: "text", text: prompt }],
      },
    });
  }

  handleLine(line: string): void {
    let evt: Record<string, unknown>;
    try {
      evt = JSON.parse(line);
    } catch {
      return;
    }

    // JSON-RPC response (id + result/error) — only the handshake ids matter.
    if (evt.id !== undefined && ("result" in evt || "error" in evt)) {
      if (evt.id === 1) {
        // initialize acked → declare initialized and open/resume the thread.
        // `developerInstructions` layers the Open Work persona on top of codex's
        // base prompt (the append-style analog of Claude's system prompt).
        void this.writeJson({ jsonrpc: "2.0", method: "initialized" });
        const params: Record<string, unknown> = {
          cwd: "/workspace",
          developerInstructions: SYSTEM_PROMPT,
        };
        if (this.ctx.resume && this.ctx.resumeId) {
          void this.writeJson({
            jsonrpc: "2.0",
            id: 2,
            method: "thread/resume",
            params: { ...params, threadId: this.ctx.resumeId },
          });
        } else {
          void this.writeJson({ jsonrpc: "2.0", id: 2, method: "thread/start", params });
        }
      } else if (evt.id === 2) {
        const result = (evt.result ?? {}) as Record<string, unknown>;
        const thread = (result.thread ?? {}) as Record<string, unknown>;
        this.threadId = (thread.id as string) || this.ctx.resumeId || "";
        this.resolveReady?.();
      }
      return;
    }

    // Notifications.
    const method = evt.method as string | undefined;
    const params = (evt.params ?? {}) as Record<string, unknown>;
    if (method === "item/agentMessage/delta") {
      const delta = params.delta;
      if (typeof delta === "string" && delta) {
        // Newlines within a message stream through in the delta itself. A new
        // agentMessage item after earlier text (a reply that resumes after a tool
        // call) → insert a paragraph break so the blocks don't run together.
        const itemId = typeof params.itemId === "string" ? params.itemId : "";
        if (this.sawText && itemId && itemId !== this.lastItemId) {
          this.ctx.publish({ type: "output", text: "\n\n" });
        }
        if (itemId) this.lastItemId = itemId;
        this.sawText = true;
        this.ctx.publish({ type: "output", text: delta });
      }
    } else if (method === "turn/completed") {
      const turn = (params.turn ?? {}) as Record<string, unknown>;
      const error = turn.error as Record<string, unknown> | null | undefined;
      if (turn.status === "failed" && error && error.message) {
        this.ctx.publish({ type: "error", message: String(error.message) });
      }
      this.ctx.publish({ type: "done" });
    }
  }

  private writeJson(obj: unknown): Promise<void> {
    return this.ctx.writeStdin(JSON.stringify(obj) + "\n");
  }
}

/** The newest codex rollout (path + thread id from its filename), or null. */
async function latestCodexRollout(
  sandbox: Sandbox,
): Promise<{ path: string; threadId: string } | null> {
  let stdout = "";
  try {
    const res = await sandbox.exec([
      "bash",
      "-lc",
      `find ${CODEX_SESSIONS_DIR} -name 'rollout-*.jsonl' -printf '%T@ %p\\n' 2>/dev/null | sort -rn | head -1`,
    ]);
    stdout = res.stdout;
  } catch {
    return null;
  }
  const path = stdout.trim().split(/\s+/).slice(1).join(" ");
  const m = path.match(CODEX_ROLLOUT_RE);
  return m ? { path, threadId: m[1] } : null;
}

/**
 * Codex app-server rollout (one JSON object per line) → messages. The clean turn
 * text lives in `event_msg` lines (`user_message`/`agent_message`); the parallel
 * `response_item` lines carry injected environment/permissions context.
 */
export function parseCodexRollout(jsonl: string): ConversationMessage[] {
  const messages: ConversationMessage[] = [];

  for (const line of jsonl.split("\n")) {
    if (!line.trim()) continue;
    let entry: Record<string, unknown>;
    try {
      entry = JSON.parse(line);
    } catch {
      continue;
    }
    if (entry.type !== "event_msg") continue;
    const payload = entry.payload as Record<string, unknown> | undefined;
    const message = payload?.message;
    if (typeof message !== "string") continue;
    const text = message.trim();
    if (!text) continue;

    if (payload!.type === "user_message") {
      if (!isSystemHint(text)) messages.push({ role: "user", content: text });
    } else if (payload!.type === "agent_message") {
      // Merge consecutive assistant messages (text before/after a tool call).
      const last = messages[messages.length - 1];
      if (last?.role === "assistant") last.content += `\n\n${text}`;
      else messages.push({ role: "assistant", content: text });
    }
  }

  return messages;
}

export const codexProvider: Provider = {
  id: "codex",
  label: "Codex",
  image: "codex",
  keyLabel: "OpenAI API key",
  env: "OPENAI_API_KEY",
  host: "api.openai.com",
  authHeader: "Authorization",
  authPrefix: "Bearer ",
  // Allow the ChatGPT-login hosts (`chatgpt.com`, `ab.chatgpt.com`) codex uses
  // when signed in with a ChatGPT account (`*.chatgpt.com` covers both), plus
  // `raw.githubusercontent.com`, which codex fetches from on startup.
  extraHosts: ["*.chatgpt.com", "chatgpt.com", "raw.githubusercontent.com", "*.github.com", "github.com"],
  models: [
    { id: "gpt-5.6-sol", label: "GPT-5.6 Sol" },
    { id: "gpt-5.6-terra", label: "GPT-5.6 Terra" },
    { id: "gpt-5.1-codex", label: "GPT-5.1 Codex" },
    { id: "gpt-5.1", label: "GPT-5.1" },
    { id: "gpt-5-codex", label: "GPT-5 Codex" },
    { id: "gpt-5", label: "GPT-5" },
  ],
  defaultModel: "gpt-5.1-codex",

  // `codex proto` was removed; the JSON-RPC-over-stdio protocol is now
  // `codex app-server` (newline-delimited JSON in/out); the driver runs its
  // handshake. `env_key` forces API-key (Bearer) auth reading OPENAI_API_KEY and
  // sends it to api.openai.com/v1 over plain HTTP/1.1, which the proxy inspects
  // (method POST) so the egress override can rewrite the placeholder to the real
  // key. `-c model=` sets the default model for new threads.
  sessionArgs: (model) => [
    "codex",
    "app-server",
    "-c",
    `model=${model}`,
    "-c",
    `model_providers.openai-http.env_key="OPENAI_API_KEY"`,
    "-c",
    `model_providers.openai-http.base_url="https://api.openai.com/v1"`,
  ],

  createDriver: (ctx) => new CodexDriver(ctx),

  resumeInfo: async (sandbox) => {
    const roll = await latestCodexRollout(sandbox);
    return { resume: !!roll, resumeId: roll?.threadId ?? "" };
  },

  readConversation: async (sandbox) => {
    const roll = await latestCodexRollout(sandbox);
    if (!roll) return [];
    try {
      return parseCodexRollout(new TextDecoder().decode(await sandbox.readFile(roll.path)));
    } catch {
      return [];
    }
  },
};
