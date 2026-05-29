import { Router, type Request, type Response } from "express";
import { listSandboxes } from "hive";
import { controllerUrl } from "../lib/controllerUrl.js";

const router = Router();

router.get("/:id/events", async (req: Request, res: Response) => {
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
    for await (const event of sandbox.getEventsStream({ signal: ac.signal, lastEventId })) {
      res.write(`id: ${event.id}\ndata: ${JSON.stringify(event)}\n\n`);
    }
  } catch {
    // stream aborted or sandbox gone — just close
  }
  res.end();
});

export default router;
