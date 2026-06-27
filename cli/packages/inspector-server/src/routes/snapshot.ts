import { Router, type Request, type Response } from "express";
import type { Snapshot } from "@hiver.sh/client";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";
import { waitForSandbox } from "../lib/waitForSandbox.js";

const router = Router();

// Capture a snapshot of the running sandbox now, without stopping it. The body
// is a Snapshot config (vm and/or files); the result reports each requested
// part independently.
router.post("/:id/:key/snapshot", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  try {
    await waitForSandbox(sandbox);
    // A live snapshot writes the full guest memory file and tars the filesystem,
    // which routinely exceeds the client's 5s default. Give it room so the call
    // isn't aborted mid-capture.
    const result = await sandbox.snapshot(req.body as Snapshot, {
      timeoutMs: 120_000,
    });
    res.json(result);
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
