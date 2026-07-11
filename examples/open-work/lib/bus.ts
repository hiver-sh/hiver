import "server-only";

import type { TaskStreamEvent } from "./types";

// A tiny in-process pub/sub. Agent sessions publish events tagged with their
// thread (session) id; the single SSE endpoint subscribes and fans them out to
// connected browsers, which route each event to the matching task.
export type Envelope = { session: string; event: TaskStreamEvent };
type Subscriber = (env: Envelope) => void;

const subscribers = new Set<Subscriber>();

export function subscribe(cb: Subscriber): () => void {
  subscribers.add(cb);
  return () => subscribers.delete(cb);
}

export function publish(session: string, event: TaskStreamEvent): void {
  for (const cb of subscribers) cb({ session, event });
}
