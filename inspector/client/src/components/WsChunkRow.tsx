import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp } from "lucide-react";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import type { SandboxEvent } from "@/types";
import { CodeViewer } from "./CodeViewer";

function tryPretty(body?: string): { content: string; isJson: boolean } | undefined {
  if (!body) return undefined;
  try { return { content: JSON.stringify(JSON.parse(body), null, 2), isJson: true }; }
  catch { return { content: body, isJson: false }; }
}

export function WsChunkRow({ chunk }: { chunk: Extract<SandboxEvent, { type: "egress.chunk" }> }) {
  const [open, setOpen] = useState(false);
  const snippet = chunk.body.length > 1000 ? chunk.body.slice(0, 1000) + "…" : chunk.body;
  const pretty = useMemo(() => tryPretty(chunk.body), [chunk.body]);

  return (
    <>
      <button
        onClick={() => setOpen(true)}
        className="flex items-center gap-2 w-full text-left px-2 py-1.5 rounded hover:bg-muted/40 transition-colors"
      >
        <div className="flex items-center gap-1 shrink-0 w-10 text-[10px] font-mono font-semibold">
          {chunk.label === "up"
            ? <><ArrowUp className="h-3 w-3 text-blue-600 dark:text-blue-400" /><span className="text-blue-600 dark:text-blue-400">up</span></>
            : chunk.label === "down"
            ? <><ArrowDown className="h-3 w-3 text-green-600 dark:text-green-400" /><span className="text-green-600 dark:text-green-400">down</span></>
            : null}
        </div>
        <span className="font-mono text-xs text-foreground/60 truncate">{snippet}</span>
      </button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-w-3xl h-[70vh] flex flex-col p-0 gap-0 overflow-hidden">
          <DialogHeader className="px-4 py-3 shrink-0 border-b border-border">
            <DialogTitle className="text-sm flex items-center gap-1.5">
              {chunk.label === "up"
                ? <><ArrowUp className="h-3.5 w-3.5 text-blue-600 dark:text-blue-400" /><span className="text-blue-600 dark:text-blue-400">up</span></>
                : chunk.label === "down"
                ? <><ArrowDown className="h-3.5 w-3.5 text-green-600 dark:text-green-400" /><span className="text-green-600 dark:text-green-400">down</span></>
                : <span>chunk</span>}
              <span className="text-muted-foreground font-normal ml-1 text-xs">WebSocket message</span>
            </DialogTitle>
          </DialogHeader>
          <div className="flex-1 min-h-0 overflow-hidden">
            <CodeViewer content={pretty?.content ?? chunk.body} lang={pretty?.isJson ? "json" : "text"} className="h-full" />
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}
