import { Router, type Request, type Response } from "express";
import { Sandbox } from "@hiver.sh/client";
import { gatewayUrl } from "../lib/gatewayUrl.js";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";
import { waitForSandbox } from "../lib/waitForSandbox.js";
import { makeLinkedSandboxRelay } from "../lib/relayLinkedSandboxEvents.js";
import {
  appendEvent,
  loadEvents,
  lastOwnEventId,
  linkedSandboxes,
} from "../lib/eventStore.js";

const router = Router();

interface TermSession {
  write(data: string): void;
  resize(cols: number, rows: number): void;
  close(): void;
}

type TermInput =
  | { type: "input"; data: string }
  | { type: "resize"; cols: number; rows: number };

interface ClientHandle {
  sendData: (buf: Buffer) => void;
  sendCtrl: (ev: string, d: object) => void;
  end: () => void;
}

interface PersistentSession {
  // null until the upstream terminal finishes opening. The session is reserved
  // in `sessions` synchronously (before the async open) so concurrent clients
  // converge on one shared upstream instead of each opening their own.
  tty: TermSession | null;
  // Resolves once `tty` is set, or rejects if the upstream open failed. Every
  // client awaits this before streaming, so they all share the same terminal.
  ready: Promise<void>;
  // Recent output, kept as a list of chunks (not one growing Buffer) so each
  // arriving chunk is an O(1) push instead of an O(scrollback) concat — the
  // latter pegs the event loop as the buffer approaches its cap, stalls the
  // upstream reader, and makes the sandbox drop the shared attach for everyone.
  scrollback: Buffer[];
  scrollbackBytes: number;
  clients: Set<ClientHandle>;
}

// Scrollback is replayed to each newly-connected client as a single SSE frame
// to restore terminal state. Keep it small: it's decoded + written to xterm on
// the browser's main thread on connect, so a multi-MB replay (easily reached by
// a busy tty entrypoint) freezes the tab. A few screens is plenty — the program
// repaints the rest on its next frame.
const MAX_SCROLLBACK = 256 * 1024; // 256 KB

const sessions = new Map<string, PersistentSession>();
const termPending = new Map<string, TermInput[]>();

async function openExecStreamSession(
  gw: string,
  sandboxId: string,
  sandboxKey: string,
  onData: (buf: Buffer) => void,
  onExit: () => void,
): Promise<TermSession> {
  const ac = new AbortController();
  const sandbox = new Sandbox(
    { id: sandboxId, key: sandboxKey },
    { gatewayUrl: gw },
  );
  // Don't open the terminal until the sandbox's server answers — attaching to a
  // sandbox that's still booting just fails the exec.
  await waitForSandbox(sandbox, { signal: ac.signal });
  const config = await sandbox.getConfig().catch(() => null);

  const cwd = config?.cwd ?? config?.fs?.[0]?.mount ?? undefined;
  const env = {
    TERM: "xterm-256color",
    COLORTERM: "truecolor",
  };
  let exec;
  if (config?.tty === true) {
    exec = await sandbox.execStream("", { tty: true, cwd, signal: ac.signal, env });
  } else {
    exec = await sandbox.execStream("/bin/sh", {
      tty: true,
      cwd,
      signal: ac.signal,
      env,
    });
  }
  exec.exitCode.catch(() => {});

  (async () => {
    for await (const pipe of exec.pipes) {
      if (pipe.stdout) onData(Buffer.from(pipe.stdout));
    }
  })()
    .catch(() => {})
    .finally(() => onExit());

  return {
    write: (d) => {
      exec.writeStdin(d).catch(() => {});
    },
    resize: (c, r) => {
      exec.writeStdin(`\x1b[8;${r};${c}t`).catch(() => {});
    },
    close: () => ac.abort(),
  };
}

// attachTerminal wires a client (its sendData/sendCtrl/end via `handle`) to the
// sandbox's single shared terminal session, opening that upstream on the first
// client and fanning out to the rest. It returns a detach function the caller
// runs when the client disconnects. Callers supply the framing (raw SSE vs the
// namespaced multiplexed frames), so this stays transport-agnostic.
export function attachTerminal(
  gw: string,
  id: string,
  key: string,
  handle: ClientHandle,
): () => void {
  // The first client for this sandbox opens the single shared upstream
  // terminal; every later client fans out from it. Reserve the session
  // synchronously so two clients connecting at once don't each open a
  // duplicate upstream — both find this entry and await its `ready`.
  let ps = sessions.get(key);
  if (!ps) {
    const created: PersistentSession = {
      tty: null,
      ready: Promise.resolve(),
      scrollback: [],
      scrollbackBytes: 0,
      clients: new Set(),
    };
    sessions.set(key, created);

    const onData = (buf: Buffer) => {
      // O(1) append; drop whole chunks off the front once over the cap. Never
      // copy the accumulated buffer here — doing so per chunk pegged the event
      // loop and stalled the upstream reader under a busy tty.
      created.scrollback.push(buf);
      created.scrollbackBytes += buf.length;
      while (
        created.scrollbackBytes > MAX_SCROLLBACK &&
        created.scrollback.length > 1
      ) {
        created.scrollbackBytes -= created.scrollback[0].length;
        created.scrollback.shift();
      }
      for (const c of created.clients) c.sendData(buf);
    };

    const onExit = () => {
      sessions.delete(key);
      for (const c of created.clients) {
        c.sendCtrl("close", {});
        c.end();
      }
      created.clients.clear();
    };

    created.ready = openExecStreamSession(gw, id, key, onData, onExit)
      .then((tty) => {
        created.tty = tty;
        // Drain input that arrived while the upstream was still opening.
        const pending = termPending.get(key);
        if (pending) {
          termPending.delete(key);
          for (const msg of pending) {
            if (msg.type === "resize") tty.resize(msg.cols, msg.rows);
            else tty.write(msg.data);
          }
        }
      })
      .catch((err) => {
        sessions.delete(key);
        throw err;
      });
    ps = created;
  }

  ps.clients.add(handle);
  let detached = false;
  const detach = () => {
    detached = true;
    sessions.get(key)?.clients.delete(handle);
    // Keep the TTY alive — the next connection replays scrollback and resumes.
  };

  ps.ready
    .then(() => {
      if (detached) return;
      // This block is synchronous, so no upstream output can interleave: the
      // client sees connected → clear → scrollback, and only subsequent live
      // output (delivered via onData) arrives afterward.
      handle.sendCtrl("connected", {});
      handle.sendData(Buffer.from("\x1b[H\x1b[2J"));
      // Concatenate once, only on connect (not per chunk), to replay scrollback.
      if (ps!.scrollback.length > 0)
        handle.sendData(Buffer.concat(ps!.scrollback));
    })
    .catch(() => {
      if (detached) return;
      handle.sendCtrl("error", { message: "no terminal available" });
      handle.end();
    });

  return detach;
}

// Multiplexed per-sandbox stream: the event feed AND terminal output over ONE
// SSE connection, so a tab holds one long-lived connection instead of two.
// Frames are namespaced by SSE event: `feed` (a SandboxEvent), `term` (base64
// terminal bytes), and `term:<ctrl>` (terminal control, e.g. connected/close).
// Terminal input still arrives via POST /terminal/input. Folding the two
// streams together keeps the browser under its ~6-per-origin HTTP/1.1
// connection cap with multiple tabs open.
router.get("/:id/:key/stream", (req: Request, res: Response) => {
  const id = req.params.id;
  const key = req.params.key;

  res.setHeader("Content-Type", "text/event-stream");
  res.setHeader("Cache-Control", "no-cache");
  res.setHeader("Connection", "keep-alive");
  res.flushHeaders();

  const write = (event: string, data: string) => {
    if (!res.writableEnded) res.write(`event: ${event}\ndata: ${data}\n\n`);
  };

  // Terminal channel: reuse the shared session, framed under `term`.
  const detach = attachTerminal(gatewayUrl(req), id, key, {
    sendData: (buf) => write("term", buf.toString("base64")),
    sendCtrl: (ev, d) => write(`term:${ev}`, JSON.stringify(d)),
    end: () => {
      if (!res.writableEnded) res.end();
    },
  });

  // Event-feed channel: proxy the sandbox's /v1/events broker, framed under
  // `feed`. Events are persisted server-side (SQLite, ~/.hiver/events.db), so
  // the browser no longer keeps its own copy or sends a lastEventId: we replay
  // everything stored on connect, then resume the upstream stream from just
  // after the last stored event so reconnects never miss or duplicate events.
  const sandbox = sandboxFromReq(req);
  const ac = new AbortController();
  (async () => {
    try {
      // Replay persisted history first so a fresh connection shows the full
      // timeline immediately, even while the sandbox is still booting.
      for (const event of loadEvents(id, key)) {
        write("feed", JSON.stringify(event));
      }
      // With nothing persisted yet, start from event 0 so we consume the
      // sandbox's full history from the beginning rather than only tailing live.
      const resumeId = lastOwnEventId(id, key) ?? 0;

      // Wait for the sandbox to be reachable before opening its event stream;
      // the abort signal bails out if the client disconnects while we wait.
      await waitForSandbox(sandbox, { signal: ac.signal });

      const relayLinked = makeLinkedSandboxRelay(
        gatewayUrl(req),
        ac.signal,
        (e) => {
          // Nested-sandbox events belong to this primary's timeline, so store
          // them under the primary owner (id, key) while keeping their own
          // sandbox identity (carried on the event) as the row's key.
          appendEvent(id, key, e);
          write("feed", JSON.stringify(e));
        },
      );

      // Resume nested sandboxes we've already recorded for this owner, even if
      // no fresh linking egress.response arrives this session — each picks up
      // from its own last persisted event.
      for (const nested of linkedSandboxes(id, key)) {
        relayLinked.openLinked(nested.id, nested.key);
      }

      for await (const event of sandbox.getEventsStream({
        signal: ac.signal,
        lastEventId: resumeId,
      })) {
        const augmented = { ...event, sandbox_id: id, sandbox_key: key };
        appendEvent(id, key, augmented);
        write("feed", JSON.stringify(augmented));
        relayLinked.relay(event);
      }
    } catch {
      // stream aborted or sandbox gone
    }
  })();

  req.on("close", () => {
    detach();
    ac.abort();
  });
});

router.post("/:id/:key/terminal/input", (req: Request, res: Response) => {
  const key = req.params.key;
  const msg = req.body as TermInput;
  const ps = sessions.get(key);

  // No session yet, or one that's reserved but still opening its upstream:
  // buffer the input; it's drained once the terminal is ready.
  if (!ps || !ps.tty) {
    const q = termPending.get(key) ?? [];
    q.push(msg);
    termPending.set(key, q);
    res.status(202).send();
    return;
  }

  if (msg.type === "resize") ps.tty.resize(msg.cols, msg.rows);
  else ps.tty.write(msg.data);
  res.status(204).send();
});

export default router;
