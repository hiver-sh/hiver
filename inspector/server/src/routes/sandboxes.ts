import { Router, type Request, type Response } from "express";
import { type SandboxConfig, getOrCreateSandbox, listSandboxes, shutdown, watchSandboxEvents } from "hive";
import { gatewayUrl } from "../lib/controllerUrl.js";

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
    res.json(sandboxes.map((s) => ({ id: s.id, exposed_endpoint: s.exposedEndpoint })));
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.put("/:id", async (req: Request, res: Response) => {
  try {
    const sandbox = await getOrCreateSandbox(
      req.params.id,
      req.body as SandboxConfig,
      { gatewayUrl: gatewayUrl(req) },
    );
    res.json({ id: sandbox.id, exposed_endpoint: sandbox.exposedEndpoint });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.post("/:id/shutdown", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ gatewayUrl: gatewayUrl(req) }).then(
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

export default router;
