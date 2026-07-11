import type { Sandbox } from "@hiver.sh/client";

import type { ConversationMessage, TaskStreamEvent } from "../types";

export type Orchestration = "claude" | "codex";

/** Bus publish bound to one thread, handed to a session driver. */
export type Publish = (event: TaskStreamEvent) => void;

/** Everything a per-session protocol driver needs from the host session. */
export interface DriverContext {
  /** Write raw bytes to the process's stdin. */
  writeStdin(data: string): Promise<void>;
  /** Emit a bus event for this thread. */
  publish: Publish;
  /** Whether a prompt is one of our hidden system hints (not a real user turn). */
  isSystemHint(text: string): boolean;
  model: string;
  /** Resume the prior conversation rather than start fresh. */
  resume: boolean;
  /** Engine-specific resume handle (e.g. a codex thread id); "" when starting fresh. */
  resumeId: string;
}

/**
 * Per-session driver translating a running agent process's stdin/stdout to bus
 * events. One instance per launched process; owns the engine's wire protocol.
 */
export interface SessionDriver {
  /** Called once, right after the process spawns (e.g. codex's handshake). */
  start(): void;
  /** Send one user turn to the process. May throw if the process is gone. */
  send(prompt: string): Promise<void>;
  /** Handle one line of the process's stdout. */
  handleLine(line: string): void;
}

/**
 * An agent engine: its metadata, egress/auth wiring, launch argv, wire protocol
 * (via {@link createDriver}), and transcript handling (resume + conversation
 * rebuild). Claude and Codex each implement this in their own file.
 */
export interface Provider {
  id: Orchestration;
  label: string;
  image: string;
  /** Human label for the API key field. */
  keyLabel: string;
  /** Env var the CLI reads the key from (a placeholder in the sandbox). */
  env: string;
  /** Provider API host the egress override injects the real key into. */
  host: string;
  /** Header the real key is written to on that host. */
  authHeader: string;
  /** Prefix for the header value (e.g. `Bearer ` for OpenAI). */
  authPrefix: string;
  /** Extra provider hosts to allow (plain `allow`, no override). */
  extraHosts?: string[];
  /** Selectable models: CLI `id` + friendly `label`. */
  models: { id: string; label: string }[];
  /** Default model id when none is chosen. */
  defaultModel: string;

  /** Build the argv for the persistent session process. */
  sessionArgs(model: string, session: { id: string; resume: boolean }): string[];

  /** Create the per-session protocol driver bound to a process's IO. */
  createDriver(ctx: DriverContext): SessionDriver;

  /**
   * Whether a resumable transcript already exists in the sandbox, plus the
   * engine-specific handle needed to resume it (`""` for Claude, whose session id
   * is fixed). The source of truth for resume-vs-start, surviving server restarts.
   */
  resumeInfo(
    sandbox: Sandbox,
    key: string,
  ): Promise<{ resume: boolean; resumeId: string }>;

  /** Rebuild the conversation from this engine's persisted transcript. */
  readConversation(sandbox: Sandbox, key: string): Promise<ConversationMessage[]>;
}
