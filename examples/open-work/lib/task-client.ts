import type { ConversationMessage, OutputFile, TaskStreamEvent } from "./types";

/** One SSE envelope: an event tagged with the thread it belongs to. */
export type StreamEnvelope = { session: string; event: TaskStreamEvent };

/**
 * Open the single shared SSE stream. Every agent event (for any thread) arrives
 * here; the caller routes each to the matching task by `session` id.
 */
export function openStream(onEnvelope: (env: StreamEnvelope) => void): () => void {
  const es = new EventSource("/api/stream");
  es.onmessage = (e) => {
    try {
      onEnvelope(JSON.parse(e.data) as StreamEnvelope);
    } catch {
      /* ignore malformed frames */
    }
  };
  return () => es.close();
}

/** Ask the server to mint a new task id (its sandbox key). */
export async function createTask(): Promise<string> {
  const res = await fetch("/api/task", { method: "POST" });
  if (!res.ok) throw new Error(`Create failed: ${res.status}`);
  const { id } = (await res.json()) as { id: string };
  return id;
}

/** Grant the task's sandbox egress to a host (user allowed the elicitation). */
export async function allowEgress(
  session: string,
  host: string,
  gatewayUrl: string,
): Promise<void> {
  await fetch("/api/sandbox", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ action: "allow-egress", key: session, host, gatewayUrl }),
  });
}

/** Delete a task: shuts down its sandbox and terminates its agent process. */
export async function deleteTaskSandbox(id: string): Promise<void> {
  try {
    await fetch(`/api/task?key=${encodeURIComponent(id)}`, { method: "DELETE" });
  } catch {
    /* best effort */
  }
}

/** Send one message. Output arrives over the shared SSE stream, not here. */
export async function sendTask(form: FormData): Promise<void> {
  const res = await fetch("/api/tasks", { method: "POST", body: form });
  if (!res.ok) throw new Error(`Send failed: ${res.status} ${res.statusText}`);
}

/** List a task's current input/output files from its sandbox (on refresh). */
export async function fetchFiles(
  session: string,
  gatewayUrl: string,
): Promise<{ inputs: OutputFile[]; outputs: OutputFile[] }> {
  try {
    const res = await fetch(
      `/api/files?session=${encodeURIComponent(session)}&gatewayUrl=${encodeURIComponent(gatewayUrl)}`,
      { cache: "no-store" },
    );
    if (!res.ok) return { inputs: [], outputs: [] };
    return (await res.json()) as { inputs: OutputFile[]; outputs: OutputFile[] };
  } catch {
    return { inputs: [], outputs: [] };
  }
}

/** Restore a thread's messages ({ role, content }) from the sandbox transcript. */
export async function fetchConversation(
  session: string,
  gatewayUrl: string,
): Promise<ConversationMessage[]> {
  try {
    const res = await fetch(
      `/api/conversation?session=${encodeURIComponent(session)}&gatewayUrl=${encodeURIComponent(gatewayUrl)}`,
      { cache: "no-store" },
    );
    if (!res.ok) return [];
    const body = (await res.json()) as { turns?: ConversationMessage[] };
    return body.turns ?? [];
  } catch {
    return [];
  }
}
