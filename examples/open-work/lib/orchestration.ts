import { claudeProvider } from "./providers/claude";
import { codexProvider } from "./providers/codex";
import type { Orchestration, Provider } from "./providers/types";

// The engine registry. Each engine's metadata, wire protocol, and transcript
// handling live in its own provider file (./providers/claude, ./providers/codex);
// this module just wires them together and re-exports the shared helpers the app
// imports. `Engine` is kept as an alias for callers that predate "provider".
export type { Orchestration, Provider } from "./providers/types";
export type Engine = Provider;
export { SYSTEM_PROMPT } from "./providers/system-prompt";
export { AGENT_SESSION_ID } from "./providers/claude";

export const ORCHESTRATIONS: Record<Orchestration, Provider> = {
  claude: claudeProvider,
  codex: codexProvider,
};

export const ORCHESTRATION_ORDER: Orchestration[] = ["claude", "codex"];

export function isOrchestration(value: string): value is Orchestration {
  return value === "claude" || value === "codex";
}

/** The orchestration a model id belongs to (used to switch engine + image). */
export function orchestrationForModel(
  modelId: string,
): Orchestration | undefined {
  return ORCHESTRATION_ORDER.find((o) =>
    ORCHESTRATIONS[o].models.some((m) => m.id === modelId),
  );
}
