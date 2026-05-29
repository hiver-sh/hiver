import { Router, type Request, type Response } from "express";
import { type SandboxConfig, getOrCreateSandbox, listSandboxes, shutdown } from "hive";
import { controllerUrl } from "../lib/controllerUrl.js";

const router = Router();

router.get("/", async (req: Request, res: Response) => {
  try {
    const sandboxes = await listSandboxes({ controllerUrl: controllerUrl(req) });
    res.json(
      sandboxes.map((s) => ({ id: s.id, endpoint: s.apiServerUrl, exposed_endpoint: s.exposedEndpoint })),
    );
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.put("/:id", async (req: Request, res: Response) => {
  try {
    const sandbox = await getOrCreateSandbox(
      req.params.id,
      req.body as SandboxConfig,
      { controllerUrl: controllerUrl(req) },
    );
    res.json({ id: sandbox.id, endpoint: sandbox.apiServerUrl, exposed_endpoint: sandbox.exposedEndpoint });
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

router.post("/:id/shutdown", async (req: Request, res: Response) => {
  try {
    const [sandbox] = await listSandboxes({ controllerUrl: controllerUrl(req) }).then(
      (list) => list.filter((s) => s.id === req.params.id),
    );
    if (!sandbox) {
      res.status(404).json({ error: "sandbox not found" });
      return;
    }
    await shutdown(sandbox);
    res.status(204).send();
  } catch (err) {
    res.status(502).json({ error: String(err) });
  }
});

export default router;
