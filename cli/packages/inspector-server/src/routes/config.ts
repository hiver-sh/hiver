import { Router, type Request, type Response } from "express";
import type { SandboxConfig } from "@hiver.sh/client";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";
import { waitForSandbox } from "../lib/waitForSandbox.js";

const router = Router();

router.get("/:id/config", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  try {
    await waitForSandbox(sandbox);
    const config = await sandbox.getConfig();
    res.json(config);
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.put("/:id/config", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  try {
    await waitForSandbox(sandbox);
    await sandbox.applyConfig(req.body as SandboxConfig);
    res.json({ ok: true });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
