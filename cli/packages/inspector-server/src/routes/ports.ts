import { Router, type Request, type Response } from "express";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";
import { waitForSandbox } from "../lib/waitForSandbox.js";

const router = Router();

router.get("/:id/ports", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  try {
    await waitForSandbox(sandbox);
    const ports = await sandbox.getPorts();
    res.json({ ports });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
