"use client";

import * as React from "react";
import { AtSign, Paperclip } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { EgressCard } from "@/components/egress-card";
import { Logo } from "@/components/logo";
import { ScrollArea } from "@/components/ui/scroll-area";
import { SmoothMarkdown } from "@/components/markdown";
import type { Task } from "@/lib/task";

export function Conversation({
  task,
  onAllowEgress,
  onDismissEgress,
}: {
  task: Task | null;
  onAllowEgress: (hosts: string[]) => void;
  onDismissEgress: (hosts: string[]) => void;
}) {
  const scrollRef = React.useRef<HTMLDivElement>(null);
  const lastOutput = task?.turns.at(-1)?.output;
  const turnCount = task?.turns.length;
  const egressCount = task?.pendingEgress.length;

  // Keep the newest output in view while it streams.
  React.useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    el.scrollTop = el.scrollHeight;
  }, [lastOutput, turnCount, egressCount, task?.id]);

  if (!task) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-1 text-center">
        <Logo size={44} className="mb-2 text-muted-foreground" />
        <span
          className="text-2xl font-semibold tracking-tight text-muted-foreground"
          style={{ fontFamily: "var(--font-display)" }}
        >
          Open Work
        </span>
        <span className="text-sm text-muted-foreground/70">
          Your agent for real work powered by the Hiver runtime
        </span>
      </div>
    );
  }

  return (
    <ScrollArea ref={scrollRef} className="flex-1">
      <div className="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-6">
        {task.turns.map((turn) => (
          <div key={turn.id} className="flex flex-col gap-6">
            {/* User's message */}
            <div className="flex flex-col items-end gap-2">
              <div className="max-w-[85%] rounded-2xl rounded-br-sm bg-primary px-4 py-2.5 text-sm text-primary-foreground whitespace-pre-wrap break-words">
                {turn.prompt}
              </div>
              {turn.files.length > 0 && (
                <div className="flex flex-wrap justify-end gap-1.5">
                  {turn.files.map((f) => (
                    <Badge key={`${f.kind}:${f.relPath}`} variant="muted" className="gap-1">
                      {f.kind === "reference" ? (
                        <AtSign className="size-3" />
                      ) : (
                        <Paperclip className="size-3" />
                      )}
                      <span className="max-w-[10rem] truncate font-mono text-[11px]">
                        {f.relPath}
                      </span>
                    </Badge>
                  ))}
                </div>
              )}
            </div>

            {/* Assistant response */}
            <div className="min-w-0">
              {turn.output.length === 0 && turn.status === "streaming" ? (
                <ThinkingDots />
              ) : (
                <SmoothMarkdown
                  content={turn.output}
                  streaming={turn.status === "streaming"}
                />
              )}
            </div>
          </div>
        ))}

        {/* Egress permission elicitation — one card for all blocked hosts. */}
        {task.pendingEgress.length > 0 && (
          <EgressCard
            hosts={task.pendingEgress}
            onAllow={onAllowEgress}
            onDismiss={onDismissEgress}
          />
        )}
      </div>
    </ScrollArea>
  );
}

function ThinkingDots() {
  return (
    <div className="flex items-center gap-1 py-1 text-muted-foreground">
      {[0, 1, 2].map((i) => (
        <span
          key={i}
          className="size-1.5 animate-bounce rounded-full bg-current"
          style={{ animationDelay: `${i * 0.15}s` }}
        />
      ))}
    </div>
  );
}
