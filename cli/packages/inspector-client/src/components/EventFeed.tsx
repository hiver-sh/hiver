import { Badge } from "@/components/ui/badge";
import { ScrollArea } from "@/components/ui/scroll-area";
import { filterEvents, type FilterState } from "@/components/TimelineView";
import type { SandboxEvent } from "@/types";

interface Props {
  events: SandboxEvent[];
  filter: FilterState;
}

type BadgeVariant =
  | "blue"
  | "green"
  | "red"
  | "purple"
  | "orange"
  | "zinc"
  | "cyan"
  | "indigo"
  | "default";

function eventBadge(event: SandboxEvent): {
  label: string;
  variant: BadgeVariant;
} {
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
    case "egress.chunk":
      return { label: "egress.chunk", variant: "cyan" };
    case "fs.request":
      return {
        label:
          `fs.req ${event.operation} ${event.access === "denied" ? "✗" : ""}`.trim(),
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
    case "resource.usage":
      return {
        label: `cpu ${event.cpu_percent.toFixed(1)}%`,
        variant: "green",
      };
    case "exec.request":
      return { label: "exec", variant: "zinc" };
    case "exec.response":
      return { label: "exec.res", variant: "zinc" };
    case "system.start":
      return { label: "start", variant: "orange" };
    case "system.config-changed":
      return { label: "config-changed", variant: "orange" };
    case "system.shutdown":
      return { label: "shutdown", variant: "orange" };
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
            <span className="text-muted-foreground">
              {event.stdout.trimEnd()}
            </span>
          ) : null}
          {event.stderr ? (
            <span className="text-muted-foreground">
              {event.stderr.trimEnd()}
            </span>
          ) : null}
        </span>
      );
    case "egress.request":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          <span className="text-zinc-500">#{event.id}</span>{" "}
          <span className="text-blue-600 dark:text-blue-400">
            {event.method}
          </span>{" "}
          {event.host}
          {event.path}
          {event.query ? `?${event.query}` : ""}
        </span>
      );
    case "egress.response":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          req#{event.request_id}{" "}
          <span
            className={
              event.status >= 400
                ? "text-red-600 dark:text-red-400"
                : "text-green-600 dark:text-green-400"
            }
          >
            {event.status}
          </span>{" "}
          {event.duration_ms}ms
        </span>
      );
    case "egress.chunk":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          req#{event.request_id} chunk
          {event.label ? (
            <span
              className={`ml-1 ${event.label === "up" ? "text-blue-600 dark:text-blue-400" : "text-green-600 dark:text-green-400"}`}
            >
              {event.label === "up" ? "↑" : "↓"} {event.label}
            </span>
          ) : null}{" "}
          ({event.body.length}b)
        </span>
      );
    case "fs.request":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          <span className="text-zinc-500">#{event.id}</span>{" "}
          <span className="text-purple-600 dark:text-purple-400">
            {event.operation}
          </span>{" "}
          {event.path}
        </span>
      );
    case "fs.response":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          req#{event.request_id} {event.backend} {event.duration_ms}ms
          {event.error ? (
            <span className="text-red-600 dark:text-red-400">
              {" "}
              {event.error}
            </span>
          ) : null}
        </span>
      );
    case "config.apply":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          {event.success
            ? "applied"
            : `failed: ${event.errorMessage ?? "unknown"}`}
        </span>
      );
    case "resource.usage":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          cpu{" "}
          <span className="text-emerald-600 dark:text-emerald-400">
            {event.cpu_percent.toFixed(1)}%
          </span>
          {" · "}mem{" "}
          <span className="text-emerald-600 dark:text-emerald-400">
            {formatBytes(event.memory_bytes)}
          </span>
        </span>
      );
    case "exec.request":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          <span className="text-zinc-500">{event.cwd}</span> {event.command}
        </span>
      );
    case "exec.response":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          req#{event.request_id}
        </span>
      );
    case "system.start":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          sandbox start requested
        </span>
      );
    case "system.config-changed":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          config updated
        </span>
      );
    case "system.shutdown":
      return (
        <span className="font-mono text-xs text-muted-foreground">
          ttl expired · shutting down
        </span>
      );
  }
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
  if (bytes < 1024 * 1024 * 1024)
    return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)}GB`;
}

export function EventFeed({ events, filter }: Props) {
  const filtered = filterEvents(events, filter);

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
        {filtered.map((event) => {
          const { label, variant } = eventBadge(event);
          const ts = new Date(event.timestamp).toISOString().slice(11, 23);
          return (
            <div
              key={event.id}
              className="flex items-center gap-2 rounded px-2 py-1 hover:bg-muted/50"
            >
              <span className="mt-0.5 shrink-0 text-muted-foreground">
                {ts}
              </span>
              <Badge variant={variant} className="mt-0.5 shrink-0">
                {label}
              </Badge>
              <EventDetail event={event} />
            </div>
          );
        })}
      </div>
    </ScrollArea>
  );
}
