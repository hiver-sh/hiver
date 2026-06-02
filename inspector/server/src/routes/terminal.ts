import { Router, type Request, type Response } from "express";
import { Sandbox } from "hive";


const router = Router();

interface TermSession {
  write(data: string): void;
  resize(cols: number, rows: number): void;
  close(): void;
}
type TermInput = { type: "input"; data: string } | { type: "resize"; cols: number; rows: number };

const termSessions = new Map<string, TermSession>();
const termPending = new Map<string, TermInput[]>();

async function openExecStreamSession(
  sandboxUrl: string,
  sandboxId: string,
  onData: (buf: Buffer) => void,
  onExit: () => void,
): Promise<TermSession> {
  const ac = new AbortController();
  const sandbox = new Sandbox({ id: sandboxId, endpoint: sandboxUrl }, {});
  const config = await sandbox.getConfig().catch(() => null);
  const cwd = config?.fs?.[0]?.mount ?? undefined;
  const env = {
    "TERM": "xterm-256color",
    "COLORTERM": "truecolor",
  };
  const exec = await sandbox.execStream("/bin/sh", { tty: true, cwd, signal: ac.signal, env });
  exec.exitCode.catch(() => {});

  (async () => {
    for await (const pipe of exec.pipes) {
      if (pipe.stdout) onData(Buffer.from(pipe.stdout));
    }
  })().catch(() => {}).finally(() => onExit());

  return {
    write: (d) => { exec.writeStdin(d).catch(() => {}); },
    resize: (c, r) => { exec.writeStdin(`\x1b[8;${r};${c}t`).catch(() => {}); },
    close: () => ac.abort(),
  };
}

router.get("/:id/terminal/stream", async (req: Request, res: Response) => {
  const sandboxUrl = req.query.sandboxUrl as string | undefined;
  if (!sandboxUrl) { res.status(400).json({ error: "missing sandboxUrl" }); return; }
  const sessionId = req.query.sessionId as string | undefined;
  if (!sessionId) { res.status(400).json({ error: "missing sessionId" }); return; }

  // Close any existing session with this id before opening a new one
  termSessions.get(sessionId)?.close();
  termSessions.delete(sessionId);

  res.setHeader("Content-Type", "text/event-stream");
  res.setHeader("Cache-Control", "no-cache");
  res.setHeader("Connection", "keep-alive");
  res.flushHeaders();

  const sendBytes = (buf: Buffer) => res.write(`data: ${buf.toString("base64")}\n\n`);
  const sendCtrl = (ev: string, d: object) => res.write(`event: ${ev}\ndata: ${JSON.stringify(d)}\n\n`);

  let ended = false;
  const onExit = () => {
    if (ended) return;
    ended = true;
    termSessions.delete(sessionId);
    sendCtrl("close", {});
    res.end();
  };

  req.on("close", () => {
    if (!ended) {
      ended = true;
      termSessions.get(sessionId)?.close();
      termSessions.delete(sessionId);
    }
  });

  let session: TermSession;
  try {
    session = await openExecStreamSession(sandboxUrl, req.params.id, sendBytes, onExit);
  } catch {
    sendCtrl("error", { message: "no terminal available" });
    res.end();
    return;
  }

  termSessions.set(sessionId, session);
  sendCtrl("connected", {});
  sendBytes(Buffer.from("\x1b[H\x1b[2J"));

  const pending = termPending.get(sessionId);
  if (pending) {
    termPending.delete(sessionId);
    for (const msg of pending) {
      if (msg.type === "resize") session.resize(msg.cols, msg.rows);
      else session.write(msg.data);
    }
  }
});

router.post("/:id/terminal/input", (req: Request, res: Response) => {
  const sessionId = req.query.sessionId as string | undefined;
  if (!sessionId) { res.status(400).json({ error: "missing sessionId" }); return; }

  const msg = req.body as TermInput;
  const session = termSessions.get(sessionId);

  if (!session) {
    const q = termPending.get(sessionId) ?? [];
    q.push(msg);
    termPending.set(sessionId, q);
    res.status(202).send();
    return;
  }

  if (msg.type === "resize") session.resize(msg.cols, msg.rows);
  else session.write(msg.data);
  res.status(204).send();
});

export default router;
