import { Router, type Request, type Response } from "express";
import {
  Sandbox,
  type SandboxConfig,
  getOrCreateSandbox,
  listSandboxes,
  watchSandboxEvents,
} from "@hiver.sh/client";
import { gatewayUrl } from "../lib/gatewayUrl.js";

const router = Router();

router.get("/events", async (req: Request, res: Response) => {
  const abort = new AbortController();
  req.on("close", () => abort.abort());

  res.setHeader("Content-Type", "text/event-stream");
  res.setHeader("Cache-Control", "no-cache");
  res.setHeader("Connection", "keep-alive");
  res.setHeader("X-Accel-Buffering", "no");
  res.flushHeaders();

  try {
    for await (const event of watchSandboxEvents(
      { gatewayUrl: gatewayUrl(req) },
      abort.signal,
    )) {
      res.write(`data: ${JSON.stringify(event)}\n\n`);
    }
  } catch {
    // upstream closed or aborted — end cleanly
  } finally {
    res.end();
  }
});

router.get("/", async (req: Request, res: Response) => {
  try {
    const sandboxes = await listSandboxes({ gatewayUrl: gatewayUrl(req) });
    res.json(sandboxes.map((s) => ({ id: s.id, key: s.key })));
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.put("/:key", async (req: Request, res: Response) => {
  try {
    const sandbox = await getOrCreateSandbox(
      req.params.key,
      req.body as SandboxConfig,
      { gatewayUrl: gatewayUrl(req) },
    );
    res.json({ id: sandbox.id, key: sandbox.key });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.post("/:id/:key/shutdown", async (req: Request, res: Response) => {
  try {
    // Shutdown is the sandbox-side DELETE /v1/<key>, routed to the pod by id; the
    // path carries both id and key, so address it directly without a list lookup.
    const gw = gatewayUrl(req);
    await new Sandbox(
      { id: req.params.id, key: req.params.key },
      { gatewayUrl: gw },
    ).shutdown();
    res.status(204).send();
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
