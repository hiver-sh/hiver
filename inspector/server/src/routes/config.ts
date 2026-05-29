import { Router, type Request, type Response } from "express";
import { type SandboxConfig, listSandboxes } from "hive";
import { controllerUrl } from "../lib/controllerUrl.js";

const router = Router();

router.get("/:id/config", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) {
      res.status(404).json({ error: "sandbox not found" });
      return;
    }
    const config = await sandbox.getConfig();
    res.json(config);
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.put("/:id/config", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) {
      res.status(404).json({ error: "sandbox not found" });
      return;
    }
    await sandbox.applyConfig(req.body as SandboxConfig);
    res.json({ ok: true });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
