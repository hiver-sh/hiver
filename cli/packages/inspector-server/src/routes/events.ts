import { Router, type Request, type Response } from "express";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";

const router = Router();

router.get("/:key/events", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);

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

export default router;
