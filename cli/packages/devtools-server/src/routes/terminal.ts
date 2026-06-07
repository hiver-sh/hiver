import { Router, type Request, type Response } from "express";
import { Sandbox } from "@hiver.sh/client";
import { gatewayUrl } from "../lib/gatewayUrl.js";

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
  tty: TermSession;
  scrollback: Buffer;
  clients: Set<ClientHandle>;
}

const sessions = new Map<string, PersistentSession>();
const termPending = new Map<string, TermInput[]>();

async function openExecStreamSession(
  gw: string,
  sandboxKey: string,
  onData: (buf: Buffer) => void,
  onExit: () => void,
  initCommand?: string,
): Promise<TermSession> {
  const ac = new AbortController();
  const sandbox = new Sandbox({ id: "", key: sandboxKey }, { gatewayUrl: gw });
  const config = await sandbox.getConfig().catch(() => null);
  const cwd = config?.fs?.[0]?.mount ?? undefined;
  const env = {
    TERM: "xterm-256color",
    COLORTERM: "truecolor",
  };
  const exec = await sandbox.execStream(initCommand ?? "/bin/sh", {
    tty: true,
    cwd,
    signal: ac.signal,
    env,
  });
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

router.get("/:key/terminal/stream", async (req: Request, res: Response) => {
  const key = req.params.key;
  const initCommand = req.query.initCommand as string | undefined;

  res.setHeader("Content-Type", "text/event-stream");
  res.setHeader("Cache-Control", "no-cache");
  res.setHeader("Connection", "keep-alive");
  res.flushHeaders();

  const sendData = (buf: Buffer) => {
    if (!res.writableEnded) res.write(`data: ${buf.toString("base64")}\n\n`);
  };
  const sendCtrl = (ev: string, d: object) => {
    if (!res.writableEnded)
      res.write(`event: ${ev}\ndata: ${JSON.stringify(d)}\n\n`);
  };

  const handle: ClientHandle = { sendData, sendCtrl, end: () => res.end() };

  const existing = sessions.get(key);

  if (existing) {
    existing.clients.add(handle);
    sendCtrl("connected", {});
    // Clear display then replay all buffered output to restore terminal state.
    sendData(Buffer.from("\x1b[H\x1b[2J"));
    if (existing.scrollback.length > 0) sendData(existing.scrollback);
  } else {
    const ps: PersistentSession = {
      tty: null as unknown as TermSession,
      scrollback: Buffer.alloc(0),
      clients: new Set([handle]),
    };

    const onData = (buf: Buffer) => {
      ps.scrollback = Buffer.concat([ps.scrollback, buf]);
      for (const c of ps.clients) c.sendData(buf);
    };

    const onExit = () => {
      sessions.delete(key);
      for (const c of ps.clients) {
        c.sendCtrl("close", {});
        c.end();
      }
      ps.clients.clear();
    };

    let tty: TermSession;
    try {
      tty = await openExecStreamSession(
        gatewayUrl(req),
        key,
        onData,
        onExit,
        initCommand,
      );
    } catch {
      sendCtrl("error", { message: "no terminal available" });
      res.end();
      return;
    }

    ps.tty = tty;
    sessions.set(key, ps);
    sendCtrl("connected", {});
    sendData(Buffer.from("\x1b[H\x1b[2J"));

    const pending = termPending.get(key);
    if (pending) {
      termPending.delete(key);
      for (const msg of pending) {
        if (msg.type === "resize") tty.resize(msg.cols, msg.rows);
        else tty.write(msg.data);
      }
    }
  }

  req.on("close", () => {
    const ps = sessions.get(key);
    if (ps) {
      ps.clients.delete(handle);
      // Keep the TTY alive — the next connection will replay scrollback and resume.
    }
  });
});

router.post("/:key/terminal/input", (req: Request, res: Response) => {
  const key = req.params.key;
  const msg = req.body as TermInput;
  const ps = sessions.get(key);

  if (!ps) {
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
