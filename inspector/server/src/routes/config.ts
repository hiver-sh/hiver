import { Router, type Request, type Response } from "express";
import type { SandboxConfig } from "hive";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";

const router = Router();

router.get("/:id/config", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  try {
    const config = await sandbox.getConfig();
    res.json(config);
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.put("/:id/config", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  try {
    await sandbox.applyConfig(req.body as SandboxConfig);
    res.json({ ok: true });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
