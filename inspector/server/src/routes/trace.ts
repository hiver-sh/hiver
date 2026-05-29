import { readFile } from "fs/promises";
import { Router, type Request, type Response } from "express";

const router = Router();

router.get("/", async (req: Request, res: Response) => {
  const filePath = req.query.path as string | undefined;
  if (!filePath) {
    res.status(400).json({ error: "path query parameter required" });
    return;
  }

  try {
    const content = await readFile(filePath, "utf8");
    const data = JSON.parse(content) as unknown;
    res.json(data);
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    res.status(500).json({ error: msg });
  }
});

export default router;
