import { CreateSandboxDialog } from "@/components/CreateSandboxDialog";
import { cn } from "@/lib/utils";
import type { SandboxRef } from "@/types";

interface Props {
  sandboxes: SandboxRef[];
  selectedId: string | null;
  selectedKey: string | null;
  connectedKey: string | null;
  onSelect: (id: string, key: string) => void;
  onCreated: (id: string, key: string) => void;
  serverUrl: string;
}

// Resolve a sandbox's presentation from its lifecycle status plus whether the
// inspector is currently streaming from it (the single "live" connection). The
// dot color is the at-rest cue; the label fades in on row hover.
function statusMeta(sb: SandboxRef, live: boolean) {
  if (live) return { label: "live", dot: "bg-green-400" };
  switch (sb.status) {
    case "start":
      return { label: "running", dot: "bg-green-400/50" };
    case "stop":
    case "die":
      return { label: "stopped", dot: "bg-yellow-400/70" };
    default:
      return { label: "", dot: "bg-muted-foreground/40" };
  }
}

export function SandboxList({
  sandboxes,
  selectedId,
  selectedKey,
  connectedKey,
  onSelect,
  onCreated,
  serverUrl,
}: Props) {
  return (
    <div className="flex h-full flex-col">
      <div className="px-2 py-3 flex">
        <CreateSandboxDialog serverUrl={serverUrl} onCreated={onCreated} />
      </div>
      <div className="scroll-container flex-1 space-y-0.5 overflow-y-auto px-2 pb-2">
        {sandboxes.map((sb) => {
          const selected = selectedId === sb.id && selectedKey === sb.key;
          const meta = statusMeta(sb, connectedKey === sb.key);
          return (
            <button
              key={sb.key}
              onClick={() => onSelect(sb.id, sb.key)}
              className={cn(
                "group relative flex w-full items-center gap-1.5 rounded-lg px-3 py-2 text-left text-sm transition-colors",
                selected
                  ? "bg-sidebar-accent text-sidebar-accent-foreground"
                  : "text-foreground/75 hover:bg-sidebar-accent/50 hover:text-foreground",
              )}
            >
              <span className="flex w-4 shrink-0 justify-center">
                <span className={cn("h-2 w-2 rounded-full", meta.dot)} />
              </span>
              <span className="truncate font-mono text-[13px]">{sb.key}</span>
              {meta.label && (
                <span className="ml-auto shrink-0 text-[10px] uppercase tracking-wider text-muted-foreground/60 opacity-0 transition-opacity group-hover:opacity-100">
                  {meta.label}
                </span>
              )}
            </button>
          );
        })}
      </div>
    </div>
  );
}
