import type {
  ConversationMessage,
  ManifestEntry,
  OutputFile,
  TaskStreamEvent,
} from "./types";

/** One request/response exchange inside a task thread. */
export type Turn = {
  id: string;
  prompt: string;
  /** Input files sent with this turn (references + attachments), for the chips. */
  files: ManifestEntry[];
  status: "streaming" | "done" | "error";
  statusLog: string[];
  output: string;
};

/** A task is a thread of turns plus its sandbox files (task-level, not per-turn). */
export type Task = {
  id: string;
  title: string;
  turns: Turn[];
  createdAt: number;
  /** Current files under /workspace/input, driven by fs events. */
  inputs: OutputFile[];
  /** Current files under /workspace/output, driven by fs events. */
  outputs: OutputFile[];
  /** Hosts the agent was denied egress to, awaiting the user's permission. */
  pendingEgress: string[];
  /** A browser (nested sandbox) is available to drive/view. */
  browser: boolean;
};

export function createTask(
  id: string,
  title: string,
  turn: Turn,
): Task {
  return {
    id,
    title,
    turns: [turn],
    createdAt: Date.now(),
    inputs: [],
    outputs: [],
    pendingEgress: [],
    browser: false,
  };
}

export function createTurn(prompt: string, files: ManifestEntry[]): Turn {
  return {
    id: crypto.randomUUID(),
    prompt,
    files,
    status: "streaming",
    statusLog: [],
    output: "",
  };
}

/** A completed turn rebuilt from the sandbox transcript on refresh. */
export function restoredTurn(prompt: string, output: string): Turn {
  return {
    id: crypto.randomUUID(),
    prompt,
    files: [],
    status: "done",
    statusLog: [],
    output,
  };
}

/** Pair restored role-tagged messages into display turns (user prompt + reply). */
export function messagesToTurns(messages: ConversationMessage[]): Turn[] {
  const turns: Turn[] = [];
  let current: Turn | null = null;
  for (const m of messages) {
    if (m.role === "user") {
      current = restoredTurn(m.content, "");
      turns.push(current);
    } else if (current) {
      current.output += (current.output ? "\n\n" : "") + m.content;
    }
  }
  return turns;
}

/** Fold a turn-level SSE event into a turn's state. File events are handled at
 * the task level (see the SSE handler), since files persist across turns. */
export function applyEvent(turn: Turn, event: TaskStreamEvent): Turn {
  switch (event.type) {
    case "status":
      return { ...turn, statusLog: [...turn.statusLog, event.message] };
    case "output":
      return { ...turn, output: turn.output + event.text };
    case "error":
      return {
        ...turn,
        status: "error",
        statusLog: [...turn.statusLog, `Error: ${event.message}`],
      };
    case "done":
      // Keep an error status if the turn already failed.
      return { ...turn, status: turn.status === "error" ? "error" : "done" };
    default:
      return turn;
  }
}

/** Apply a task-level event (files, egress) to a task. */
export function applyTaskEvent(task: Task, event: TaskStreamEvent): Task {
  if (event.type === "file") {
    const key = event.role === "input" ? "inputs" : "outputs";
    if (task[key].some((f) => f.path === event.file.path)) return task; // dedupe
    return { ...task, [key]: [...task[key], event.file] };
  }
  if (event.type === "file-removed") {
    const key = event.role === "input" ? "inputs" : "outputs";
    return { ...task, [key]: task[key].filter((f) => f.path !== event.path) };
  }
  if (event.type === "egress-denied") {
    const added = event.hosts.filter((h) => !task.pendingEgress.includes(h));
    if (added.length === 0) return task; // all already pending
    return { ...task, pendingEgress: [...task.pendingEgress, ...added] };
  }
  if (event.type === "browser") {
    return task.browser === event.ready ? task : { ...task, browser: event.ready };
  }
  return task;
}

export function clearPendingEgress(task: Task, host: string): Task {
  return { ...task, pendingEgress: task.pendingEgress.filter((h) => h !== host) };
}

export function taskTitle(prompt: string): string {
  const firstLine = prompt.trim().split("\n")[0];
  return firstLine.length > 60 ? `${firstLine.slice(0, 57)}…` : firstLine || "Untitled task";
}
