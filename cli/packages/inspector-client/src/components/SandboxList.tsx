import {
  Activity,
  Camera,
  Check,
  ChevronRight,
  FolderTree,
  Globe,
  LayoutGrid,
  MoreVertical,
  Plug,
  Power,
  SlidersHorizontal,
  SquareTerminal,
} from "lucide-react";
import { useState } from "react";
import { CreateSandboxDialog } from "@/components/CreateSandboxDialog";
import { PortUsageDialog } from "@/components/PortUsageDialog";
import { SandboxConfigDialog } from "@/components/SandboxConfigDialog";
import { SnapshotDialog } from "@/components/SnapshotDialog";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { useTransport } from "@/lib/transport";
import { useUserPreferences } from "@/lib/userPreferences";
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

// Resolve a sandbox's dot color from its lifecycle status plus whether the
// inspector is currently streaming from it (the single "live" connection).
function statusDot(sb: SandboxRef, live: boolean) {
  if (live) return "bg-green-400";
  switch (sb.status) {
    case "start":
      return "bg-green-400/50";
    case "stop":
    case "die":
      return "bg-yellow-400/70";
    default:
      return "bg-muted-foreground/40";
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
  const { transport, gatewayUrl } = useTransport();
  // The panel toggles below flip the global view prefs shared with the open
  // detail view (there's a single set of visible panels at a time).
  const { prefs, setPref } = useUserPreferences();
  // Which row's action menu is open, and which sandbox each dialog targets.
  // Tracked by key (globally unique) since in pack mode many sandboxes share
  // one pod id.
  const [menuKey, setMenuKey] = useState<string | null>(null);
  const [snapshotFor, setSnapshotFor] = useState<SandboxRef | null>(null);
  const [configFor, setConfigFor] = useState<SandboxRef | null>(null);
  const [connectFor, setConnectFor] = useState<SandboxRef | null>(null);

  const panelToggles = [
    { label: "Timeline", icon: Activity, key: "showTimeline" as const },
    { label: "Terminal", icon: SquareTerminal, key: "showTerminal" as const },
    { label: "Browser", icon: Globe, key: "showBrowser" as const },
    { label: "Files", icon: FolderTree, key: "showFiles" as const },
  ];

  async function handleShutdown(sb: SandboxRef) {
    if (!confirm(`Shut down sandbox "${sb.key}"?`)) return;
    // Fire and forget: useSandboxLifecycleEvents streams the real status
    // (stop/die) and drops the sandbox from the list once it's gone.
    try {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sb.id)}/${encodeURIComponent(sb.key)}/shutdown`,
      );
      await transport.fetch(url, { method: "POST" });
    } catch {
      // Ignore — the lifecycle stream is the source of truth for status.
    }
  }

  return (
    <div className="flex h-full flex-col">
      <div className="px-2 py-3 flex">
        <CreateSandboxDialog serverUrl={serverUrl} onCreated={onCreated} />
      </div>
      <div className="scroll-container flex-1 space-y-0.5 overflow-y-auto px-2 pb-2">
        {sandboxes.map((sb) => {
          const selected = selectedId === sb.id && selectedKey === sb.key;
          const dot = statusDot(sb, connectedKey === sb.key);
          const menuOpen = menuKey === sb.key;
          return (
            <div
              key={sb.key}
              className={cn(
                "group relative flex w-full items-center gap-1.5 rounded-lg px-3 py-2 text-left text-sm transition-colors",
                selected
                  ? "bg-sidebar-accent text-sidebar-accent-foreground"
                  : "text-foreground/75 hover:bg-sidebar-accent/50 hover:text-foreground",
              )}
            >
              <button
                onClick={() => onSelect(sb.id, sb.key)}
                className="flex min-w-0 flex-1 items-center gap-1.5 text-left"
              >
                <span className="flex w-4 shrink-0 justify-center">
                  <span className={cn("h-2 w-2 rounded-full", dot)} />
                </span>
                <span className="truncate font-mono text-[13px]">{sb.key}</span>
              </button>
              <Popover
                open={menuOpen}
                onOpenChange={(open) => setMenuKey(open ? sb.key : null)}
              >
                <PopoverTrigger asChild>
                  <button
                    title="Actions"
                    className={cn(
                      "-mr-1 ml-auto flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-muted-foreground/60 transition-colors hover:bg-foreground/10 hover:text-foreground",
                      menuOpen
                        ? "opacity-100"
                        : "opacity-0 group-hover:opacity-100",
                    )}
                  >
                    <MoreVertical className="h-4 w-4" />
                  </button>
                </PopoverTrigger>
                <PopoverContent align="end" className="w-44 p-1 bg-sidebar">
                  {/* Panels submenu — a hover flyout. The toggles are global
                      view prefs, so the menu stays open to let several be
                      flipped at once. */}
                  <div className="group/panels relative">
                    <button className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm transition-colors hover:bg-sidebar-accent/60 group-hover/panels:bg-sidebar-accent/60">
                      <LayoutGrid className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                      Panels
                      <ChevronRight className="ml-auto h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                    </button>
                    {/* pl-1 keeps the hoverable area flush with the trigger's
                        right edge so the pointer never crosses an empty gap. */}
                    <div className="invisible absolute left-full top-0 z-50 pl-1 opacity-0 group-hover/panels:visible group-hover/panels:opacity-100">
                      <div className="w-40 rounded-md border border-border bg-sidebar p-1 shadow-md">
                        {panelToggles.map(({ label, icon: Icon, key }) => (
                          <button
                            key={key}
                            onClick={() => setPref(key, !prefs[key])}
                            className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-sidebar-accent/60 transition-colors"
                          >
                            <Check
                              className={cn(
                                "h-3.5 w-3.5 shrink-0",
                                prefs[key] ? "opacity-100" : "opacity-0",
                              )}
                            />
                            <Icon className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                            {label}
                          </button>
                        ))}
                      </div>
                    </div>
                  </div>
                  <div className="my-1 h-px bg-border" />
                  <button
                    onClick={() => {
                      setMenuKey(null);
                      setConnectFor(sb);
                    }}
                    className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-sidebar-accent/60 transition-colors"
                  >
                    <Plug className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                    Connect
                  </button>
                  <button
                    onClick={() => {
                      setMenuKey(null);
                      setSnapshotFor(sb);
                    }}
                    className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-sidebar-accent/60 transition-colors"
                  >
                    <Camera className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                    Capture snapshot
                  </button>
                  <button
                    onClick={() => {
                      setMenuKey(null);
                      setConfigFor(sb);
                    }}
                    className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-sidebar-accent/60 transition-colors"
                  >
                    <SlidersHorizontal className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                    Update Config
                  </button>
                  <div className="my-1 h-px bg-border" />
                  <button
                    onClick={() => {
                      setMenuKey(null);
                      handleShutdown(sb);
                    }}
                    className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-sidebar-accent/60 transition-colors"
                  >
                    <Power className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                    Shutdown
                  </button>
                </PopoverContent>
              </Popover>
            </div>
          );
        })}
      </div>

      {connectFor && (
        <PortUsageDialog
          sandboxId={connectFor.id}
          sandboxKey={connectFor.key}
          gatewayUrl={gatewayUrl}
          port={null}
          open={true}
          onOpenChange={(open) => {
            if (!open) setConnectFor(null);
          }}
        />
      )}

      {snapshotFor && (
        <SnapshotDialog
          sandboxId={snapshotFor.id}
          sandboxKey={snapshotFor.key}
          serverUrl={serverUrl}
          gatewayUrl={gatewayUrl}
          open={true}
          onOpenChange={(open) => {
            if (!open) setSnapshotFor(null);
          }}
        />
      )}

      {configFor && (
        <SandboxConfigDialog
          sandboxId={configFor.id}
          sandboxKey={configFor.key}
          serverUrl={serverUrl}
          open={true}
          onOpenChange={(open) => {
            if (!open) setConfigFor(null);
          }}
        />
      )}
    </div>
  );
}
