import cors from "cors";
import express, { type Request, type Response } from "express";
import { createServer } from "http";
import { WebSocketServer, type WebSocket } from "ws";
import { spawn as ptySpawn } from "node-pty";
import { Client as SshClient } from "ssh2";
import {
  DEFAULT_CONTROLLER_URL,
  type SandboxConfig,
  getOrCreateSandbox,
  getSandbox,
  listSandboxes,
  shutdown,
} from "hive";

const app = express();
const PORT = process.env.PORT ? parseInt(process.env.PORT) : 3001;
const DEFAULT_URL =
  process.env.CONTROLLER_URL ?? DEFAULT_CONTROLLER_URL;

app.use(cors());
app.use(express.json());

function controllerUrl(req: Request): string {
  const override =
    (req.query.controller as string | undefined) ??
    req.headers["x-controller-url"];
  return typeof override === "string" && override.length > 0
    ? override
    : DEFAULT_URL;
}

// GET /api/sandboxes — list all running sandboxes
app.get("/api/sandboxes", async (req: Request, res: Response) => {
  try {
    const sandboxes = await listSandboxes({ controllerUrl: controllerUrl(req) });
    res.json(
      sandboxes.map((s) => ({ id: s.id, endpoint: s.apiServerUrl, exposed_endpoint: s.exposedEndpoint })),
    );
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

// PUT /api/sandboxes/:id — create or get a sandbox
app.put("/api/sandboxes/:id", async (req: Request, res: Response) => {
  try {
    const sandbox = await getOrCreateSandbox(
      req.params.id,
      req.body as SandboxConfig,
      { controllerUrl: controllerUrl(req) },
    );
    res.json({ id: sandbox.id, endpoint: sandbox.apiServerUrl, exposed_endpoint: sandbox.exposedEndpoint });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

// POST /api/sandboxes/:id/shutdown — stop a sandbox
app.post("/api/sandboxes/:id/shutdown", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) {
      res.status(404).json({ error: "sandbox not found" });
      return;
    }
    await shutdown(sandbox);
    res.status(204).send();
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

// GET /api/sandboxes/:id/config — read current config
app.get("/api/sandboxes/:id/config", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) {
      res.status(404).json({ error: "sandbox not found" });
      return;
    }
    const config = await sandbox.getConfig();
    res.json(config);
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

// PUT /api/sandboxes/:id/config — apply a new config
app.put("/api/sandboxes/:id/config", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) {
      res.status(404).json({ error: "sandbox not found" });
      return;
    }
    await sandbox.applyConfig(req.body as SandboxConfig);
    res.json({ ok: true });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

// GET /api/sandboxes/:id/directories?path=... — list directory contents
app.get("/api/sandboxes/:id/directories", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) { res.status(404).json({ error: "sandbox not found" }); return; }
    const path = req.query.path as string | undefined;
    if (!path) { res.status(400).json({ error: "missing query param: path" }); return; }
    const entries = await sandbox.listDirectory(path);
    res.json({ entries });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

// GET /api/sandboxes/:id/file?path=... — download a file
app.get("/api/sandboxes/:id/file", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) { res.status(404).json({ error: "sandbox not found" }); return; }
    const path = req.query.path as string | undefined;
    if (!path) { res.status(400).json({ error: "missing query param: path" }); return; }
    const bytes = await sandbox.downloadFile(path);
    const filename = path.split("/").pop() ?? "file";
    res.setHeader("Content-Disposition", `attachment; filename="${filename}"`);
    res.setHeader("Content-Type", "application/octet-stream");
    res.send(Buffer.from(bytes));
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

// GET /api/sandboxes/:id/events — SSE stream of sandbox events
app.get("/api/sandboxes/:id/events", async (req: Request, res: Response) => {
  const controller = controllerUrl(req);
  let sandboxes: Awaited<ReturnType<typeof listSandboxes>>;
  try {
    sandboxes = await listSandboxes({ controllerUrl: controller });
  } catch (err) {
    res.status(502).json({ error: String(err) });
    return;
  }

  const sandbox = sandboxes.find((s) => s.id === req.params.id);
  if (!sandbox) {
    res.status(404).json({ error: "sandbox not found" });
    return;
  }

  res.setHeader("Content-Type", "text/event-stream");
  res.setHeader("Cache-Control", "no-cache");
  res.setHeader("Connection", "keep-alive");
  res.flushHeaders();

  const ac = new AbortController();
  req.on("close", () => ac.abort());

  const lastEventIdParam = req.query.lastEventId as string | undefined;
  const lastEventId = lastEventIdParam ? parseInt(lastEventIdParam) : undefined;

  try {
    for await (const event of sandbox.getEventsStream({
      signal: ac.signal,
      lastEventId,
    })) {
      res.write(`id: ${event.id}\ndata: ${JSON.stringify(event)}\n\n`);
    }
  } catch {
    // stream aborted or sandbox gone — just close
  }
  res.end();
});

// WS /api/sandboxes/:id/terminal — SSH first, fall back to terminal_cmd
const httpServer = createServer(app);
const wss = new WebSocketServer({ noServer: true });

httpServer.on("upgrade", async (req, socket, head) => {
  const url = new URL(req.url ?? "/", "http://localhost");
  const match = url.pathname.match(/^\/api\/sandboxes\/([^/]+)\/terminal$/);
  if (!match) {
    socket.destroy();
    return;
  }

  const sandboxId = match[1];
  const ctrlUrl = url.searchParams.get("controller") ?? DEFAULT_URL;

  let detail: Awaited<ReturnType<typeof getSandbox>>;
  try {
    detail = await getSandbox(sandboxId, { controllerUrl: ctrlUrl });
  } catch (e) {
    console.error("getSandbox failed:", e);
    socket.destroy();
    return;
  }

  wss.handleUpgrade(req, socket, head, async (ws) => {
    const pending: string[] = [];
    const collectPending = (msg: Buffer) => pending.push(msg.toString());
    ws.on("message", collectPending);

    if (detail.exposed_endpoint) {
      const colonIdx = detail.exposed_endpoint.lastIndexOf(":");
      const sshHost = detail.exposed_endpoint.slice(0, colonIdx);
      const sshPort = parseInt(detail.exposed_endpoint.slice(colonIdx + 1));
      const connected = await trySshTerminal(ws, sshHost, sshPort, url.searchParams, pending);
      if (connected) return;
    }

    ws.off("message", collectPending);
    if (detail.terminal_cmd) {
      handleHostTerminal(ws, detail.terminal_cmd.split(" "), url.searchParams, pending);
    } else {
      ws.close();
    }
  });
});

function handleHostTerminal(ws: WebSocket, cmd: string[], params: URLSearchParams, prePending: string[] = []) {
  let cols = Math.max(1, parseInt(params.get("cols") ?? "80"));
  let rows = Math.max(1, parseInt(params.get("rows") ?? "24"));

  const pending = [...prePending];
  ws.on("message", (msg) => pending.push(msg.toString()));

  const pty = ptySpawn("/bin/sh", ["-c", cmd.join(" ")], {
    name: "xterm-256color", cols, rows, env: process.env as Record<string, string>,
  });

  function handleMessage(text: string) {
    try {
      const ctrl = JSON.parse(text);
      if (ctrl.type === "resize" && typeof ctrl.cols === "number" && typeof ctrl.rows === "number") {
        cols = ctrl.cols;
        rows = ctrl.rows;
        pty.resize(cols, rows);
        return;
      }
    } catch { /* raw input */ }
    pty.write(text);
  }

  ws.send(JSON.stringify({ type: "connected" }));
  ws.removeAllListeners("message");
  for (const msg of pending) handleMessage(msg);
  pending.length = 0;
  ws.on("message", (msg) => handleMessage(msg.toString()));

  pty.onData((data) => { if (ws.readyState === ws.OPEN) ws.send(Buffer.from(data)); });
  pty.onExit(() => ws.close());
  ws.on("close", () => pty.kill());
}


function trySshTerminal(ws: WebSocket, host: string, port: number, params: URLSearchParams, pending: string[]): Promise<boolean> {
  return new Promise((resolve) => {
    let cols = Math.max(1, parseInt(params.get("cols") ?? "80"));
    let rows = Math.max(1, parseInt(params.get("rows") ?? "24"));
    let resolved = false;
    const conn = new SshClient();

    conn.on("ready", () => {
      conn.shell({ term: "xterm-256color", cols, rows }, (err, stream) => {
        if (err) {
          conn.end();
          if (!resolved) { resolved = true; resolve(false); }
          return;
        }

        resolved = true;
        resolve(true);

        function handleMessage(text: string) {
          try {
            const ctrl = JSON.parse(text);
            if (ctrl.type === "resize" && typeof ctrl.cols === "number" && typeof ctrl.rows === "number") {
              cols = ctrl.cols;
              rows = ctrl.rows;
              stream.setWindow(rows, cols, 0, 0);
              return;
            }
          } catch { /* raw input */ }
          stream.write(text);
        }

        ws.send(JSON.stringify({ type: "connected" }));
        ws.removeAllListeners("message");
        for (const msg of pending) handleMessage(msg);
        pending.length = 0;
        ws.on("message", (msg) => handleMessage(msg.toString()));

        stream.on("data", (data: Buffer) => { if (ws.readyState === ws.OPEN) ws.send(data); });
        stream.stderr?.on("data", (data: Buffer) => { if (ws.readyState === ws.OPEN) ws.send(data); });
        stream.on("close", () => { ws.close(); conn.end(); });
        ws.on("close", () => { stream.close(); conn.end(); });
      });
    });

    conn.on("error", (err) => {
      if (!resolved) {
        resolved = true;
        resolve(false);
      } else if (ws.readyState === ws.OPEN) {
        if (!err.message.includes("before handshake")) {
          ws.send(`\r\nSSH error: ${err.message}\r\n`);
        }
        ws.close();
      }
    });

    conn.connect({ host, port, username: "agent", password: "agent", readyTimeout: 10000, hostVerifier: () => true });
  });
}

httpServer.listen(PORT, () => {
  console.log(`Inspector server on http://localhost:${PORT}`);
  console.log(`Default controller: ${DEFAULT_URL}`);
});
