import { Router, type Request, type Response } from "express";
import { listSandboxes } from "hive";
import { controllerUrl } from "../lib/controllerUrl.js";

const router = Router();

router.get("/:id/directories", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) { res.status(404).json({ error: "sandbox not found" }); return; }
    const path = req.query.path as string | undefined;
    if (!path) { res.status(400).json({ error: "missing query param: path" }); return; }
    const entries = await sandbox.listDirectory(path);
    res.json({ entries });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.get("/:id/file", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) { res.status(404).json({ error: "sandbox not found" }); return; }
    const path = req.query.path as string | undefined;
    if (!path) { res.status(400).json({ error: "missing query param: path" }); return; }
    const bytes = await sandbox.downloadFile(path);
    const filename = path.split("/").pop() ?? "file";
    res.setHeader("Content-Disposition", `attachment; filename="${filename}"`);
    res.setHeader("Content-Type", "application/octet-stream");
    res.send(Buffer.from(bytes));
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
