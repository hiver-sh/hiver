import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp } from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import type { SandboxEvent } from "@/types";
import { CodeViewer } from "./CodeViewer";
import { tryPretty } from "@/lib/prettyBody";
import { formatWallClock, humanDuration } from "@/lib/utils";

type Chunk = Extract<SandboxEvent, { type: "egress.chunk" | "ingress.chunk" }>;

// One row per streamed message. WebSocket chunks carry an up/down label for the
// frame direction; SSE chunks are always server→client so the proxy leaves the
// label empty and we drop the direction column entirely.
export function ChunkRow({
  chunk,
  kind,
  startedAt,
}: {
  chunk: Chunk;
  kind: "ws" | "sse";
  // Request timestamp, used to show each message's offset into the stream.
  startedAt?: string;
}) {
  const [open, setOpen] = useState(false);
  const snippet =
    chunk.body.length > 1000 ? chunk.body.slice(0, 1000) + "…" : chunk.body;
  const pretty = useMemo(() => tryPretty(chunk.body), [chunk.body]);
  const clock = formatWallClock(chunk.timestamp);
  const offsetMs = useMemo(() => {
    if (!startedAt) return null;
    const delta =
      new Date(chunk.timestamp).getTime() - new Date(startedAt).getTime();
    return Number.isFinite(delta) ? delta : null;
  }, [chunk.timestamp, startedAt]);

  const direction =
    chunk.label === "up" ? (
      <>
        <ArrowUp className="h-3 w-3 text-blue-600 dark:text-blue-400" />
        <span className="text-blue-600 dark:text-blue-400">up</span>
      </>
    ) : chunk.label === "down" ? (
      <>
        <ArrowDown className="h-3 w-3 text-green-600 dark:text-green-400" />
        <span className="text-green-600 dark:text-green-400">down</span>
      </>
    ) : null;

  return (
    <>
      <button
        onClick={() => setOpen(true)}
        className="flex items-center gap-2 w-full text-left px-2 py-1.5 rounded hover:bg-muted/40 transition-colors"
      >
        {kind === "ws" && (
          <div className="flex items-center gap-1 shrink-0 w-10 text-[10px] font-mono font-semibold">
            {direction}
          </div>
        )}
        {clock && (
          <span
            className="shrink-0 font-mono text-[10px] text-muted-foreground/60 tabular-nums"
            title={chunk.timestamp}
          >
            {clock}
          </span>
        )}
        <span className="font-mono text-xs text-foreground/60 truncate">
          {snippet}
        </span>
      </button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-w-3xl h-[70vh] flex flex-col p-0 gap-0 overflow-hidden">
          <DialogHeader className="px-4 py-3 shrink-0 border-b border-border">
            <DialogTitle className="text-sm flex items-center gap-1.5">
              {kind === "ws" ? (
                direction ?? <span>chunk</span>
              ) : (
                <span>message</span>
              )}
              <span className="text-muted-foreground font-normal ml-1 text-xs">
                {kind === "ws" ? "WebSocket message" : "SSE message"}
              </span>
              {clock && (
                <span
                  className="text-muted-foreground/60 font-normal font-mono text-[11px] tabular-nums ml-auto mr-6"
                  title={chunk.timestamp}
                >
                  {clock}
                  {offsetMs != null && offsetMs >= 0 && (
                    <span className="ml-2">+{humanDuration(offsetMs)}</span>
                  )}
                </span>
              )}
            </DialogTitle>
          </DialogHeader>
          <div className="flex-1 min-h-0 overflow-hidden">
            <CodeViewer
              content={pretty?.content ?? chunk.body}
              lang={pretty?.isJson ? "json" : "text"}
              className="h-full"
            />
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}
