import { SandboxConfig } from "@hiver.sh/client";
import { select } from "../prompt.js";

export const AGENTS = [
  { label: "Claude Code", value: "claude" },
  { label: "Codex", value: "codex" },
  { label: "GitHub Copilot", value: "copilot" },
  { label: "Gemini CLI", value: "gemini" },
] as const;

export type AgentEntrypoint = (typeof AGENTS)[number]["value"];

export async function selectAgentEntrypoint(): Promise<AgentEntrypoint> {
  return select<AgentEntrypoint>("Which agent would you like to launch?", [
    ...AGENTS,
  ]);
}

export function applyAgentCliDefaults(config: SandboxConfig): void {
  config.tty = true;
  config.cwd = "/workspace";
}
