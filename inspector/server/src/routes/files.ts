import { Router, type Request, type Response } from "express";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";

const router = Router();

router.get("/:key/directories", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  const path = req.query.path as string | undefined;
  if (!path) { res.status(400).json({ error: "missing query param: path" }); return; }
  try {
    const entries = await sandbox.listDirectory(path);
    res.json({ entries });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.get("/:key/file", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  const path = req.query.path as string | undefined;
  if (!path) { res.status(400).json({ error: "missing query param: path" }); return; }
  try {
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
