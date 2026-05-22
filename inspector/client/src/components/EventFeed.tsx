import { useEffect, useRef } from "react";
import { Badge } from "@/components/ui/badge";
import { ScrollArea } from "@/components/ui/scroll-area";
import type { SandboxEvent } from "@/types";

interface Props {
  events: SandboxEvent[];
  autoScroll: boolean;
}

type BadgeVariant =
  | "blue" | "green" | "red" | "purple" | "orange"
  | "zinc" | "cyan" | "indigo" | "default";

function eventBadge(event: SandboxEvent): { label: string; variant: BadgeVariant } {
  switch (event.type) {
    case "stdio":
      return { label: "stdio", variant: "zinc" };
    case "egress.request":
      return {
        label: `egress.req ${event.access === "denied" ? "✗" : ""}`.trim(),
        variant: event.access === "denied" ? "red" : "blue",
      };
    case "egress.response":
      return {
        label: `egress.res ${event.status}`,
        variant: event.status >= 400 ? "red" : "green",
      };
    case "egress.stream_chunk":
      return { label: "egress.chunk", variant: "cyan" };
    case "fs.request":
      return {
        label: `fs.req ${event.operation} ${event.access === "denied" ? "✗" : ""}`.trim(),
        variant: event.access === "denied" ? "red" : "purple",
      };
    case "fs.response":
      return {
        label: `fs.res ${event.error ? "✗" : ""}`.trim(),
        variant: event.error ? "red" : "indigo",
      };
    case "config.apply":
      return {
        label: `config ${event.success ? "✓" : "✗"}`,
        variant: event.success ? "orange" : "red",
      };
    default:
      return { label: "unknown", variant: "default" };
  }
}

function EventDetail({ event }: { event: SandboxEvent }) {
  switch (event.type) {
    case "stdio":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          {event.stdout ? (
            <span className="text-muted-foreground">{event.stdout.trimEnd()}</span>
          ) : null}
          {event.stderr ? (
            <span className="text-muted-foreground">{event.stderr.trimEnd()}</span>
          ) : null}
        </span>
      );
    case "egress.request":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          <span className="text-zinc-500">#{event.id}</span>{" "}
          <span className="text-blue-400">{event.method}</span>{" "}
          {event.host}{event.path}
          {event.query ? `?${event.query}` : ""}
          {event.body ? <span className="text-zinc-400"> {event.body}</span> : null}
        </span>
      );
    case "egress.response":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          req#{event.request_id}{" "}
          <span className={event.status >= 400 ? "text-red-400" : "text-green-400"}>
            {event.status}
          </span>{" "}
          {event.duration_ms}ms
        </span>
      );
    case "egress.stream_chunk":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          req#{event.request_id} chunk ({event.body.length}b)
        </span>
      );
    case "fs.request":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          <span className="text-zinc-500">#{event.id}</span>{" "}
          <span className="text-purple-400">{event.operation}</span>{" "}
          {event.path}
        </span>
      );
    case "fs.response":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          req#{event.request_id} {event.backend} {event.duration_ms}ms
          {event.error ? <span className="text-red-400"> {event.error}</span> : null}
        </span>
      );
    case "config.apply":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          {event.success ? "applied" : `failed: ${event.errorMessage ?? "unknown"}`}
        </span>
      );
  }
}

export function EventFeed({ events, autoScroll }: Props) {
  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (autoScroll) {
      bottomRef.current?.scrollIntoView({ behavior: "smooth" });
    }
  }, [events, autoScroll]);

  if (events.length === 0) {
    return (
      <div className="flex h-full items-center justify-center text-muted-foreground text-sm">
        Waiting for events…
      </div>
    );
  }

  return (
    <ScrollArea className="h-full">
      <div className="space-y-0.5 p-2 font-mono text-xs">
        {events.map((event) => {
          const { label, variant } = eventBadge(event);
          const ts = new Date(event.timestamp).toISOString().slice(11, 23);
          return (
            <div
              key={event.id}
              className="flex items-center gap-2 rounded px-2 py-1 hover:bg-muted/50"
            >
              <span className="mt-0.5 shrink-0 text-muted-foreground">{ts}</span>
              <Badge variant={variant} className="mt-0.5 shrink-0">
                {label}
              </Badge>
              <EventDetail event={event} />
            </div>
          );
        })}
        <div ref={bottomRef} />
      </div>
    </ScrollArea>
  );
}
