import { Router, type Request, type Response } from "express";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";

const router = Router();

router.get("/:key/ports", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  try {
    const ports = await sandbox.getPorts();
    res.json({ ports });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
