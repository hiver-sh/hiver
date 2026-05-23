import { Box, RefreshCw } from "lucide-react";
import { CreateSandboxDialog } from "@/components/CreateSandboxDialog";
import { cn } from "@/lib/utils";
import type { SandboxRef } from "@/types";

interface Props {
  sandboxes: SandboxRef[];
  selectedId: string | null;
  loading: boolean;
  onSelect: (id: string) => void;
  onRefresh: () => void;
  serverUrl: string;
  controllerUrl: string;
}

export function SandboxList({
  sandboxes,
  selectedId,
  loading,
  onSelect,
  onRefresh,
  serverUrl,
  controllerUrl,
}: Props) {
  return (
    <div className="flex h-full flex-col">
      <div className="px-5 py-3 flex">
        <CreateSandboxDialog
          serverUrl={serverUrl}
          controllerUrl={controllerUrl}
          onCreated={onRefresh}
        />
      </div>
      <div className="flex items-center justify-between px-5 py-2">
        <span className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
          Sandboxes
        </span>
        <button
          title="Refresh"
          onClick={onRefresh}
          disabled={loading}
          className="text-muted-foreground/50 hover:text-muted-foreground transition-colors"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", loading && "animate-spin")} />
        </button>
      </div>

      {sandboxes.length === 0 && !loading ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-2 px-5 text-center text-sm text-muted-foreground">
          <Box className="h-8 w-8 opacity-30" />
          <p>No sandboxes running</p>
        </div>
      ) : (
        <div className="flex-1 overflow-y-auto">
          {sandboxes.map((sb) => (
            <button
              key={sb.id}
              onClick={() => onSelect(sb.id)}
              className={cn(
                "flex w-full items-center gap-2 px-5 py-2 text-left text-sm transition-colors hover:bg-accent",
                selectedId === sb.id && "bg-accent text-accent-foreground",
              )}
            >
              <span
                className={cn(
                  "mt-0.5 h-2 w-2 shrink-0 rounded-full",
                  selectedId === sb.id
                    ? "bg-green-400"
                    : "bg-muted-foreground/40",
                )}
              />
              <span className="truncate font-mono">{sb.id}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
