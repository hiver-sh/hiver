import { ChevronDown } from "lucide-react";
import { useState } from "react";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";

export type AnyConfig = Record<string, unknown>;

type Template = {
  label: string;
  idPrefix?: string;
  apply: (cfg: AnyConfig) => AnyConfig;
};

export const TEMPLATE_GROUPS: { group: string; templates: Template[] }[] = [
  {
    group: "Images",
    templates: [
      {
        label: "Claude Code",
        idPrefix: "claude-code",
        apply: () => ({
          image: "claude",
          entrypoint: "claude",
          cwd: "/workspace",
          tty: true,
          snapshot: {
            vm: {
              key: "claude-code-vm",
            },
            files: {
              key: "claude-code-files",
              write_on_shutdown: true,
              include: ["/workspace/*", "/home/agent/.claude/*", "/home/agent/.claude.json"],
            },
          },
        }),
      },
      {
        label: "Codex",
        idPrefix: "codex",
        apply: () => ({
          image: "codex",
          entrypoint: "codex",
          cwd: "/workspace",
          tty: true,
          snapshot: {
            vm: {
              key: "codex-vm"
            },
            files: {
              key: "codex-files",
              write_on_shutdown: true,
              include: ["/workspace/*", "/home/agent/.codex/*"],
            },
          },
        }),
      },
      {
        label: "OpenClaw",
        idPrefix: "openclaw",
        apply: () => ({
          image: "openclaw",
          cwd: "/workspace",
          env: {
            ANTHROPIC_API_KEY: "<enter>",
            OPENCLAW_GATEWAY_PASSWORD: "hiver-openclaw",
          },
          snapshot: {
            vm: {
              key: "openclaw-vm",
            },
            files: {
              key: "openclaw-files",
              write_on_shutdown: true,
              include: ["/workspace/*", "/home/agent/*"],
            },
          },
        }),
      },
      {
        label: "GitHub Copilot",
        idPrefix: "copilot",
        apply: () => ({
          image: "copilot",
          entrypoint: "copilot",
          cwd: "/workspace",
          tty: true,
          snapshot: {
            vm: {
              key: "copilot-vm",
            },
            files: {
              key: "copilot-files",
              write_on_shutdown: true,
              include: ["/workspace/*", "/home/agent/*"],
            },
          },
        }),
      },
      {
        label: "Google Antigravity",
        idPrefix: "antigravity",
        apply: () => ({
          image: "antigravity",
          entrypoint: "agy",
          cwd: "/workspace",
          tty: true,
          snapshot: {
            vm: {
              key: "antigravity-vm",
            },
            files: {
              key: "antigravity-files",
              write_on_shutdown: true,
              include: ["/workspace/*", "/home/agent/*"],
            },
          },
        }),
      },
      {
        label: "Web browser",
        idPrefix: "browser",
        apply: () => ({
          image: "browser",
          tty: true,
          snapshot: {
            vm: {
              key: "browser-vm",
            },
            files: {
              key: "browser-files",
              write_on_shutdown: true,
              include: ["/opt/hiver/chrome-profile/*"],
            },
          },
        }),
      },
      {
        label: "Node.js",
        idPrefix: "nodejs",
        apply: () => ({
          image: "node",
          entrypoint: "node",
          cwd: "/workspace",
          tty: true,
          snapshot: {
            vm: {
              key: "node-vm",
            },
          },
        }),
      },
      {
        label: "Python",
        idPrefix: "python",
        apply: () => ({
          image: "python",
          entrypoint: "python",
          cwd: "/workspace",
          tty: true,
          snapshot: {
            vm: {
              key: "python-vm",
            },
          },
        }),
      },
    ],
  },
];

interface Props {
  disabled?: boolean;
  onApply: (template: (cfg: AnyConfig) => AnyConfig) => void;
  onSuggestId?: (id: string) => void;
}

export function SandboxConfigTemplates({
  disabled,
  onApply,
  onSuggestId,
}: Props) {
  const [open, setOpen] = useState(false);
  const groups = TEMPLATE_GROUPS;
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          disabled={disabled}
          className="flex items-center gap-1 rounded-md border border-border px-2 py-0.5 text-xs text-muted-foreground transition-colors hover:bg-muted/40 disabled:opacity-50"
        >
          Use template
          <ChevronDown className="h-3 w-3" />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-48 p-1">
        {groups.map((group, gi) => (
          <div key={gi}>
            {gi > 0 && <div className="my-1 border-t border-border" />}
            <p className="px-2 py-1 text-[10px] font-medium uppercase tracking-wider text-muted-foreground/60">
              {group.group}
            </p>
            {group.templates.map((t, ti) => (
              <button
                key={ti}
                type="button"
                className="w-full rounded-sm px-2 py-1.5 text-left text-xs hover:bg-muted/60"
                onClick={() => {
                  onApply(t.apply);
                  if (t.idPrefix && onSuggestId) {
                    onSuggestId(
                      `${t.idPrefix}-${Math.random().toString(36).slice(2, 4)}`,
                    );
                  }
                  setOpen(false);
                }}
              >
                {t.label}
              </button>
            ))}
          </div>
        ))}
      </PopoverContent>
    </Popover>
  );
}
