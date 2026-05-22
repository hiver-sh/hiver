import cors from "cors";
import express, { type Request, type Response } from "express";
import { createServer } from "http";
import { spawn } from "child_process";
import { WebSocketServer, type WebSocket } from "ws";
import { Client as SshClient } from "ssh2";
import {
  DEFAULT_CONTROLLER_URL,
  type SandboxConfig,
  getOrCreateSandbox,
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
      sandboxes.map((s) => ({ id: s.id, endpoint: s.apiServerUrl })),
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
    res.json({ id: sandbox.id, endpoint: sandbox.apiServerUrl });
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

// WS /api/sandboxes/:id/terminal — attach to the sandbox's tmux session over SSH
const httpServer = createServer(app);
const wss = new WebSocketServer({ noServer: true });

httpServer.on("upgrade", (req, socket, head) => {
  const url = new URL(req.url ?? "/", "http://localhost");
  const match = url.pathname.match(/^\/api\/sandboxes\/([^/]+)\/terminal$/);
  if (!match) {
    socket.destroy();
    return;
  }
  wss.handleUpgrade(req, socket, head, (ws) => {
    handleTerminal(ws, decodeURIComponent(match[1]), url.searchParams);
  });
});

function lookupDockerPort(container: string, port: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = spawn("docker", ["port", container, `${port}/tcp`], {
      stdio: ["inherit", "pipe", "inherit"],
    });
    let out = "";
    child.stdout?.on("data", (d: Buffer) => (out += d.toString()));
    child.once("error", reject);
    child.once("exit", (code) => {
      if (code !== 0) return reject(new Error(`docker port exit ${code}`));
      const line = out.trim().split("\n").find((l) => l.startsWith("0.0.0.0:"));
      if (!line) return reject(new Error(`no IPv4 mapping for port ${port}`));
      resolve(line.split(":")[1]!.trim());
    });
  });
}

async function handleTerminal(ws: WebSocket, id: string, params: URLSearchParams) {
  let cols = Math.max(1, parseInt(params.get("cols") ?? "80"));
  let rows = Math.max(1, parseInt(params.get("rows") ?? "24"));

  const containerName = `hive-sandbox-${id}`;
  let sshPort: string;
  try {
    sshPort = await lookupDockerPort(containerName, 22);
  } catch (err) {
    ws.send(`\r\nError: could not find SSH port for ${containerName}: ${err}\r\n`);
    ws.close();
    return;
  }

  // Buffer messages that arrive before the SSH shell is open.
  const pending: string[] = [];
  ws.on("message", (msg) => pending.push(msg.toString()));

  const conn = new SshClient();

  conn.on("ready", () => {
    conn.shell({ term: "xterm-256color", cols, rows }, (err, stream) => {
      if (err) {
        ws.send(`\r\nSSH shell error: ${err}\r\n`);
        ws.close();
        conn.end();
        return;
      }

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

      // Switch to live message handling and flush anything buffered.
      ws.removeAllListeners("message");
      for (const msg of pending) handleMessage(msg);
      pending.length = 0;
      ws.on("message", (msg) => handleMessage(msg.toString()));

      stream.on("data", (data: Buffer) => {
        if (ws.readyState === ws.OPEN) ws.send(data);
      });
      stream.stderr?.on("data", (data: Buffer) => {
        if (ws.readyState === ws.OPEN) ws.send(data);
      });
      stream.on("close", () => {
        ws.close();
        conn.end();
      });

      ws.on("close", () => {
        stream.close();
        conn.end();
      });
    });
  });

  conn.on("error", (err) => {
    if (ws.readyState === ws.OPEN) {
      ws.send(`\r\nSSH error: ${err.message}\r\n`);
      ws.close();
    }
  });

  conn.connect({
    host: "127.0.0.1",
    port: parseInt(sshPort),
    username: "claude-agent",
    password: "root",
    readyTimeout: 10000,
    hostVerifier: () => true,
  });
}

httpServer.listen(PORT, () => {
  console.log(`Inspector server on http://localhost:${PORT}`);
  console.log(`Default controller: ${DEFAULT_URL}`);
});
