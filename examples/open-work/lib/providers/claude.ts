import type { Sandbox } from "@hiver.sh/client";

import { isSystemHint } from "../system-message";
import type { ConversationMessage } from "../types";
import { SYSTEM_PROMPT } from "./system-prompt";
import type { DriverContext, Provider, SessionDriver } from "./types";

// Each task's sandbox runs Claude under one fixed session id, so the transcript
// path is stable and `--resume` picks up the same conversation across restarts.
export const AGENT_SESSION_ID = "0a0a0a0a-0000-4000-8000-000000000000";

const TRANSCRIPT_PATH = `/home/agent/.claude/projects/-workspace/${AGENT_SESSION_ID}.jsonl`;

/** Drives Claude Code over stream-json: user turns in, event stream out. */
class ClaudeDriver implements SessionDriver {
  private sawText = false; // any text emitted this turn (for paragraph breaks)
  private msgStreamed = false; // current assistant message arrived via deltas

  constructor(private ctx: DriverContext) {}

  start(): void {
    // Claude reads stream-json turns straight off stdin — nothing to kick off.
  }

  async send(prompt: string): Promise<void> {
    // A system hint continues the current turn (keep the separator context);
    // a real user turn starts fresh.
    if (!this.ctx.isSystemHint(prompt)) this.sawText = false;
    this.msgStreamed = false;
    const message = { type: "user", message: { role: "user", content: prompt } };
    await this.ctx.writeStdin(JSON.stringify(message) + "\n");
  }

  handleLine(line: string): void {
    let evt: Record<string, unknown>;
    try {
      evt = JSON.parse(line);
    } catch {
      return;
    }

    switch (evt.type) {
      case "stream_event": {
        const e = (evt.event ?? {}) as Record<string, unknown>;
        if (e.type === "message_start") this.msgStreamed = false;
        if (e.type === "content_block_delta") {
          const delta = (e.delta ?? {}) as Record<string, unknown>;
          if (delta.type === "text_delta" && typeof delta.text === "string") {
            this.sawText = true;
            this.msgStreamed = true;
            this.ctx.publish({ type: "output", text: delta.text });
          }
        }
        if (e.type === "content_block_start") {
          const block = (e.content_block ?? {}) as Record<string, unknown>;
          if (block.type === "tool_use" && typeof block.name === "string") {
            this.ctx.publish({ type: "status", message: `Using ${block.name}…` });
          }
          // A new text block after prior text (reply resumes after a tool call)
          // → paragraph break so they don't run together.
          if (block.type === "text" && this.sawText) {
            this.ctx.publish({ type: "output", text: "\n\n" });
          }
        }
        break;
      }
      case "assistant": {
        // Non-streamed message (no deltas) → publish it whole, separated from any
        // prior text. Streamed messages already went out via deltas.
        if (!this.msgStreamed) {
          const text = assistantText(evt.message as Record<string, unknown>);
          if (text) {
            if (this.sawText) this.ctx.publish({ type: "output", text: "\n\n" });
            this.ctx.publish({ type: "output", text });
            this.sawText = true;
          }
        }
        this.msgStreamed = false;
        break;
      }
      case "result":
        this.ctx.publish({ type: "done" });
        break;
    }
  }
}

function assistantText(message: Record<string, unknown> | undefined): string {
  const content = message?.content;
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .filter((b) => (b as Record<string, unknown>).type === "text")
      .map((b) => (b as Record<string, unknown>).text as string)
      .join("");
  }
  return "";
}

async function readTranscript(sandbox: Sandbox): Promise<string> {
  try {
    return new TextDecoder().decode(await sandbox.readFile(TRANSCRIPT_PATH));
  } catch {
    return ""; // no transcript yet
  }
}

/** Claude Code stream-json transcript (one JSON object per line) → messages. */
export function parseClaudeTranscript(jsonl: string): ConversationMessage[] {
  const messages: ConversationMessage[] = [];

  for (const line of jsonl.split("\n")) {
    if (!line.trim()) continue;
    let entry: Record<string, unknown>;
    try {
      entry = JSON.parse(line);
    } catch {
      continue;
    }
    const message = entry.message as { content?: unknown } | undefined;
    if (!message) continue;

    if (entry.type === "user") {
      // Skip injected/meta user messages (skill files, system reminders), tool
      // results, and our own hidden system hints — only real user prompts count.
      if (entry.isMeta || typeof message.content !== "string") continue;
      const text = message.content.trim();
      if (text && !isSystemHint(text)) messages.push({ role: "user", content: text });
    } else if (entry.type === "assistant") {
      const text = assistantText(message);
      if (!text) continue;
      // Merge consecutive assistant messages (text before/after a tool call).
      const last = messages[messages.length - 1];
      if (last?.role === "assistant") last.content += `\n\n${text}`;
      else messages.push({ role: "assistant", content: text });
    }
  }

  return messages;
}

export const claudeProvider: Provider = {
  id: "claude",
  label: "Claude Code",
  image: "claude",
  keyLabel: "Anthropic API key",
  env: "ANTHROPIC_API_KEY",
  host: "api.anthropic.com",
  authHeader: "x-api-key",
  authPrefix: "",
  models: [
    { id: "claude-sonnet-5", label: "Sonnet 5" },
    { id: "claude-opus-4-8", label: "Opus 4.8" },
    { id: "claude-fable-5", label: "Fable 5" },
  ],
  defaultModel: "claude-sonnet-5",

  sessionArgs: (model, session) => [
    "claude",
    "-p",
    "--model",
    model,
    "--append-system-prompt",
    SYSTEM_PROMPT,
    // Stable session id → resume the same conversation across restarts.
    ...(session.resume ? ["--resume", session.id] : ["--session-id", session.id]),
    "--input-format",
    "stream-json",
    "--output-format",
    "stream-json",
    "--verbose",
    "--include-partial-messages",
  ],

  createDriver: (ctx) => new ClaudeDriver(ctx),

  resumeInfo: async (sandbox) => ({
    resume: (await readTranscript(sandbox)).length > 0,
    resumeId: "",
  }),

  readConversation: async (sandbox) =>
    parseClaudeTranscript(await readTranscript(sandbox)),
};
