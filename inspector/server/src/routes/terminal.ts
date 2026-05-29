import { Router, type Request, type Response } from "express";
import { Client as SshClient } from "ssh2";

const router = Router();

interface TermSession {
  write(data: string): void;
  resize(cols: number, rows: number): void;
  close(): void;
}
type TermInput = { type: "input"; data: string } | { type: "resize"; cols: number; rows: number };

const termSessions = new Map<string, TermSession>();
const termPending = new Map<string, TermInput[]>();

function openSshSession(
  host: string, port: number, cols: number, rows: number,
  onData: (buf: Buffer) => void, onExit: () => void,
): Promise<TermSession | null> {
  return new Promise((resolve) => {
    const conn = new SshClient();
    let resolved = false;
    conn.on("ready", () => {
      conn.shell({ term: "xterm-256color", cols, rows }, (err, stream) => {
        if (err) { conn.end(); if (!resolved) { resolved = true; resolve(null); } return; }
        resolved = true;
        stream.on("data", (d: Buffer) => onData(d));
        stream.stderr?.on("data", (d: Buffer) => onData(d));
        stream.on("close", () => { conn.end(); onExit(); });
        resolve({
          write: (d) => stream.write(d),
          resize: (c, r) => stream.setWindow(r, c, 0, 0),
          close: () => { stream.close(); conn.end(); },
        });
      });
    });
    conn.on("error", (err) => {
      if (!resolved) { resolved = true; resolve(null); }
      else onData(Buffer.from(`\r\nSSH error: ${err.message}\r\n`));
    });
    conn.connect({ host, port, username: "agent", password: "agent", readyTimeout: 10000, hostVerifier: () => true });
  });
}

router.get("/:id/terminal/stream", async (req: Request, res: Response) => {
  const sandboxUrl = req.query.sandboxUrl as string | undefined;
  if (!sandboxUrl) { res.status(400).json({ error: "missing sandboxUrl" }); return; }
  const sessionId = req.query.sessionId as string | undefined;
  if (!sessionId) { res.status(400).json({ error: "missing sessionId" }); return; }

  const cols = Math.max(1, parseInt((req.query.cols as string) || "80"));
  const rows = Math.max(1, parseInt((req.query.rows as string) || "24"));

  const exposedEndpoint = req.query.exposedBackend as string | undefined;

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

  let session: TermSession | null = null;

  if (exposedEndpoint) {
    const i = exposedEndpoint.lastIndexOf(":");
    session = await openSshSession(
      exposedEndpoint.slice(0, i),
      parseInt(exposedEndpoint.slice(i + 1)),
      cols, rows, sendBytes, onExit,
    );
  }

  if (!session) {
    sendCtrl("error", { message: "no terminal available" });
    res.end();
    return;
  }

  termSessions.set(sessionId, session);
  sendCtrl("connected", {});

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
