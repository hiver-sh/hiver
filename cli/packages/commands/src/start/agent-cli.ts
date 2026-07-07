import { select } from "../prompt.js";

export const IMAGES = [
  { label: "Claude Code", value: "claude" },
  { label: "Codex", value: "codex" },
  { label: "Copilot", value: "copilot" },
  { label: "OpenClaw", value: "openclaw" },
  { label: "Web browser", value: "browser" },
  { label: "Node.js", value: "node" },
  { label: "Python", value: "python" },
] as const;

export type ImageName = (typeof IMAGES)[number]["value"];

export async function selectImage(): Promise<ImageName> {
  return select<ImageName>("Which image would you like to launch?", [
    ...IMAGES,
  ]);
}
