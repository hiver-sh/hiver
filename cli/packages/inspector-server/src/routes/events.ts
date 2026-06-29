import { Router, type Request, type Response } from "express";
import type { SandboxEvent } from "@hiver.sh/client";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";
import { waitForSandbox } from "../lib/waitForSandbox.js";
import { gatewayUrl } from "../lib/gatewayUrl.js";
import { makeLinkedSandboxRelay } from "../lib/relayLinkedSandboxEvents.js";

const router = Router();

router.get("/:id/:key/events", async (req: Request, res: Response) => {
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
    // Don't open the upstream stream until the sandbox's server is up — a
    // freshly created/resuming sandbox isn't ready to stream immediately, and
    // connecting too early just errors out. The abort signal stops the wait if
    // the client disconnects in the meantime.
    await waitForSandbox(sandbox, { signal: ac.signal });

    const sendEvent = (
      event: SandboxEvent & { sandbox_id: string; sandbox_key: string },
    ) => res.write(`id: ${event.id}\ndata: ${JSON.stringify(event)}\n\n`);

    const relayLinked = makeLinkedSandboxRelay(
      gatewayUrl(req),
      ac.signal,
      sendEvent,
    );

    for await (const event of sandbox.getEventsStream({
      signal: ac.signal,
      lastEventId,
    })) {
      sendEvent({
        ...event,
        sandbox_id: req.params.id,
        sandbox_key: req.params.key,
      });
      relayLinked(event);
    }
  } catch {
    // stream aborted or sandbox gone — just close
  }
  res.end();
});

export default router;
