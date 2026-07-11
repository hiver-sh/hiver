import "server-only";

import { Sandbox, type ExecProcess } from "@hiver.sh/client";

import { publish } from "./bus";
import {
  addSandboxDependency,
  browserKeyFor,
  currentGatewayUrl,
  getSandbox,
  isViewableFile,
  skipEgressPermission,
  type Provisioning,
} from "./hiver";
import { ORCHESTRATIONS, type Orchestration } from "./orchestration";
import type { SessionDriver } from "./providers/types";
import { isSystemHint } from "./system-message";

// Blocked egress hosts arrive in bursts (e.g. a single page load fans out to
// many hosts). Rather than pop a card per host, collect them per thread and emit
// once the burst settles — debounced on any egress activity, capped so steady
// traffic still flushes. `shown` dedupes across the whole session (and across the
// main + nested sandbox watchers, which share a threadId).
const EGRESS_DEBOUNCE_MS = 700;
const EGRESS_MAX_WAIT_MS = 3000;

type EgressBatch = {
  shown: Set<string>;
  pending: Set<string>;
  timer: ReturnType<typeof setTimeout> | null;
  firstAt: number;
};
const egressBatches = new Map<string, EgressBatch>();

function egressBatch(threadId: string): EgressBatch {
  let b = egressBatches.get(threadId);
  if (!b) {
    b = { shown: new Set(), pending: new Set(), timer: null, firstAt: 0 };
    egressBatches.set(threadId, b);
  }
  return b;
}

function scheduleEgressFlush(threadId: string, b: EgressBatch) {
  if (b.pending.size === 0) return;
  if (b.timer) clearTimeout(b.timer);
  // Debounce on activity, but never wait longer than the cap since the first host.
  const wait = Math.max(
    0,
    Math.min(EGRESS_DEBOUNCE_MS, EGRESS_MAX_WAIT_MS - (Date.now() - b.firstAt)),
  );
  b.timer = setTimeout(() => {
    b.timer = null;
    if (b.pending.size === 0) return;
    const hosts = [...b.pending];
    for (const h of hosts) b.shown.add(h);
    b.pending.clear();
    publish(threadId, { type: "egress-denied", hosts });
  }, wait);
}

/** Queue a newly blocked host for the thread's batched elicitation card. */
function queueEgressDenied(threadId: string, host: string) {
  const b = egressBatch(threadId);
  if (b.shown.has(host) || b.pending.has(host)) return; // already surfaced/queued
  if (b.pending.size === 0) b.firstAt = Date.now();
  b.pending.add(host);
  scheduleEgressFlush(threadId, b);
}

/** Any egress request extends the collection window (until the burst settles). */
function noteEgressActivity(threadId: string) {
  const b = egressBatches.get(threadId);
  if (b) scheduleEgressFlush(threadId, b);
}

/** Drop a thread's batch state (on task teardown). */
export function clearEgressBatch(threadId: string) {
  const b = egressBatches.get(threadId);
  if (b?.timer) clearTimeout(b.timer);
  egressBatches.delete(threadId);
}

/**
 * Tail a sandbox's events and relay them under the task's thread: egress denials
 * become permission elicitations, file writes under /workspace/input|output
 * become file events, and any nested sandbox the agent spawns (revealed by an
 * egress.response's x-hiver-sandbox-id/key headers) is tracked and recursively
 * watched — so its events surface as if they came from the main sandbox.
 */
async function watchSandbox(
  sandbox: Sandbox,
  threadId: string,
  signal: AbortSignal,
  seen: Set<string>,
  isMain: boolean,
): Promise<void> {
  const pending = new Map<number, { path: string; op: "write" | "delete" }>();
  try {
    for await (const evt of sandbox.getEventsStream({ signal })) {
      // Egress requests are collected into one debounced card: blocked hosts (bar
      // known telemetry/noise) are queued; any request keeps the window open so a
      // burst is gathered before the card is shown.
      if (evt.type === "egress.request") {
        if (evt.access === "denied" && !skipEgressPermission(evt.host)) {
          queueEgressDenied(threadId, evt.host);
        } else {
          noteEgressActivity(threadId);
        }
        continue;
      }

      // An egress response carrying x-hiver-sandbox-id/key means the agent
      // spawned a nested sandbox → track it and relay ITS events too (recursive,
      // so a chain a→b→c is fully surfaced).
      if (evt.type === "egress.response" && evt.headers) {
        const lower = Object.fromEntries(
          Object.entries(evt.headers).map(([k, v]) => [k.toLowerCase(), v]),
        );
        const id = lower["x-hiver-sandbox-id"];
        const depKey = lower["x-hiver-sandbox-key"];
        if (id && depKey && !seen.has(id)) {
          seen.add(id);
          addSandboxDependency(threadId, id, depKey);
          // The browser VM → tell the client a browser viewer is available.
          if (depKey === browserKeyFor(threadId)) {
            publish(threadId, { type: "browser", ready: true });
          }
          const linked = new Sandbox(
            { id, key: depKey },
            { gatewayUrl: currentGatewayUrl() },
          );
          void watchSandbox(linked, threadId, signal, seen, false);
        }
        continue;
      }
      // File input/output tracking is only for the main sandbox's /workspace;
      // nested sandboxes (e.g. the browser VM) only relay egress + nesting.
      if (!isMain) continue;
      if (evt.type === "fs.request") {
        if (
          (evt.operation === "write" || evt.operation === "delete") &&
          evt.access === "allowed"
        ) {
          pending.set(evt.id, { path: evt.path, op: evt.operation });
        }
      } else if (evt.type === "fs.response") {
        const req = pending.get(evt.request_id);
        pending.delete(evt.request_id);
        if (!req || evt.error) continue;
        const role = req.path.startsWith("/workspace/output/")
          ? "output"
          : req.path.startsWith("/workspace/input/")
            ? "input"
            : null;
        if (!role) continue;
        const name = req.path.split("/").pop() || req.path;
        if (!isViewableFile(name)) continue;
        if (req.op === "delete") {
          publish(threadId, { type: "file-removed", role, path: req.path });
        } else {
          publish(threadId, { type: "file", role, file: { name, path: req.path } });
        }
      }
    }
  } catch {
    /* aborted or stream ended */
  }
}

// One persistent agent process per task thread, driven over stdin/stdout as
// stream-json. Events are published to the bus keyed by the thread id; the SSE
// endpoint delivers them to the browser. The API key rides on the sandbox's
// egress override, so no env is passed here.

class AgentSession {
  private buf = "";
  private stderr = "";
  private alive = true;
  // The engine's wire protocol (stream-json for Claude, app-server for Codex)
  // lives in a per-provider driver; this session just pumps stdout lines into it.
  private driver!: SessionDriver;

  private constructor(
    private proc: ExecProcess,
    private controller: AbortController,
    readonly threadId: string,
    readonly image: string,
    readonly provKey: string,
    readonly model: string,
  ) {}

  static async create(
    threadId: string,
    image: string,
    provisioning: Provisioning,
    model: string,
    provKey: string,
    args: string[],
    resume: boolean,
    resumeId: string,
  ): Promise<AgentSession> {
    const sandbox = await getSandbox(threadId, image, provisioning);
    const controller = new AbortController();
    const proc = await sandbox.execStream(args, {
      cwd: "/workspace",
      signal: controller.signal,
    });
    const session = new AgentSession(
      proc,
      controller,
      threadId,
      image,
      provKey,
      model,
    );
    // The provider owns the wire protocol; give its driver an IO context bound to
    // this process and thread.
    const provider = ORCHESTRATIONS[image as Orchestration] ?? ORCHESTRATIONS.claude;
    session.driver = provider.createDriver({
      writeStdin: (data) => proc.writeStdin(data),
      publish: (event) => publish(threadId, event),
      isSystemHint,
      model,
      resume,
      resumeId,
    });
    session.pump();
    session.driver.start();
    // Mark dead as soon as the process exits, so a reuse race doesn't write to
    // a dead stdin (the `-p` process can exit after finishing a turn).
    void proc.exitCode.then(
      () => (session.alive = false),
      () => (session.alive = false),
    );
    // Relay the sandbox's events (files, egress, nested sandboxes) to the task.
    void watchSandbox(sandbox, threadId, controller.signal, new Set(), true);
    return session;
  }

  get isAlive() {
    return this.alive;
  }

  /** Send one user turn; its events flow to the bus, not back to the caller.
   * Throws if the process isn't running, so the caller can restart + retry. */
  async send(prompt: string): Promise<void> {
    if (!this.alive) throw new Error("Agent session is not running");
    try {
      await this.driver.send(prompt);
    } catch (err) {
      // Process is gone; mark dead so getSession restarts (resuming) on retry.
      this.alive = false;
      throw err;
    }
  }

  /** Terminate the process (frees the Claude session id for a resumed relaunch). */
  stop() {
    this.alive = false;
    try {
      this.controller.abort();
    } catch {
      /* already aborted */
    }
  }

  private async pump() {
    try {
      for await (const pipe of this.proc.pipes) {
        if (pipe.stderr) this.stderr += pipe.stderr;
        if (!pipe.stdout) continue;
        this.buf += pipe.stdout;
        let nl: number;
        while ((nl = this.buf.indexOf("\n")) !== -1) {
          const line = this.buf.slice(0, nl).trim();
          this.buf = this.buf.slice(nl + 1);
          if (line) this.driver.handleLine(line);
        }
      }
    } catch {
      /* aborted or connection dropped */
    } finally {
      const wasAlive = this.alive;
      this.alive = false;
      // Surface an unexpected exit (not a deliberate stop) as an error + done.
      if (wasAlive && this.stderr.trim()) {
        publish(this.threadId, { type: "error", message: this.stderr.trim() });
        publish(this.threadId, { type: "done" });
      }
    }
  }
}

// Registry: one session per task thread, keyed by the client's task id.
const sessions = new Map<string, AgentSession>();

/**
 * Get the thread's session, (re)starting the process when the model changed or
 * it died. On a restart within the same sandbox the on-disk session store is
 * intact, so the new process resumes the conversation (`buildArgs(true)`).
 */
export async function getSession(
  threadId: string,
  image: string,
  provisioning: Provisioning,
  model: string,
  buildArgs: (resume: boolean) => string[],
): Promise<AgentSession> {
  const provKey = JSON.stringify(provisioning);
  const existing = sessions.get(threadId);

  if (
    existing &&
    existing.isAlive &&
    existing.image === image &&
    existing.provKey === provKey &&
    existing.model === model
  ) {
    return existing;
  }

  existing?.stop();

  // Resume iff a prior transcript exists in the sandbox — the source of truth,
  // since the in-memory registry is lost on server restart / hot-reload /
  // snapshot restore. Each provider reports whether it has one and the handle to
  // resume it (Claude by its fixed session id, Codex by the rollout's thread id).
  const provider = ORCHESTRATIONS[image as Orchestration] ?? ORCHESTRATIONS.claude;
  const sandbox = await getSandbox(threadId, image, provisioning);
  const { resume, resumeId } = await provider.resumeInfo(sandbox, threadId);

  const session = await AgentSession.create(
    threadId,
    image,
    provisioning,
    model,
    provKey,
    buildArgs(resume),
    resume,
    resumeId,
  );
  sessions.set(threadId, session);
  return session;
}

/** Terminate and forget a thread's session process (used when deleting a task). */
export function stopSession(threadId: string): void {
  sessions.get(threadId)?.stop();
  sessions.delete(threadId);
  clearEgressBatch(threadId);
}
