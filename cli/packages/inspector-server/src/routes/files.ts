import { Router, type Request, type Response } from "express";
import { sandboxFromReq } from "../lib/sandboxFromReq.js";
import { waitForSandbox } from "../lib/waitForSandbox.js";

const router = Router();

router.get("/:id/directories", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  const path = req.query.path as string | undefined;
  if (!path) {
    res.status(400).json({ error: "missing query param: path" });
    return;
  }
  try {
    // Only the root listing races sandbox boot (it's the first call the file
    // explorer makes); a request for a subdirectory means the tree already
    // loaded, so the sandbox is known reachable — skip the extra ping.
    if (path === "/") await waitForSandbox(sandbox);
    const entries = await sandbox.listDirectory(path);
    res.json({ entries });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.get("/:id/file", async (req: Request, res: Response) => {
  const sandbox = sandboxFromReq(req);
  const path = req.query.path as string | undefined;
  if (!path) {
    res.status(400).json({ error: "missing query param: path" });
    return;
  }
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
