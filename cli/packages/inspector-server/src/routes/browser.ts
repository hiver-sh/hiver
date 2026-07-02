import { Router, type Request, type Response } from "express";
import {
  browserInput,
  browserControl,
  browserGetSelection,
  type BrowserInput,
  type BrowserControl,
} from "../lib/cdp.js";

const router = Router();

// These routes address the sandbox that owns the browser — which may be a nested
// sandbox, not the primary the panel is under. The client learns that id/key
// from the `browser:connected` frame on the shared stream and targets it here,
// so the session lookup (keyed by that sandbox's key) resolves correctly.

// Forward a single input event (mouse / key / text) to the shared CDP session,
// mirroring POST /terminal/input. The screencast session is opened by the
// stream handler; if it isn't attached yet the event is simply dropped (the
// user retries by moving/clicking again — there's no meaningful buffer for
// pointer state).
router.post("/:id/:key/browser/input", (req: Request, res: Response) => {
  const key = req.params.key;
  browserInput(key, req.body as BrowserInput);
  res.status(204).send();
});

// Navigate the current page, or open a new one, from the address bar.
router.post("/:id/:key/browser/control", (req: Request, res: Response) => {
  const key = req.params.key;
  void browserControl(key, req.body as BrowserControl).catch(() => {});
  res.status(202).send();
});

// The active page's current selection text, so the client can copy it to the
// local clipboard (paste is the reverse: the client sends the clipboard text as
// a `text` input, which becomes Input.insertText).
router.get("/:id/:key/browser/selection", async (req: Request, res: Response) => {
  const key = req.params.key;
  try {
    res.json({ text: await browserGetSelection(key) });
  } catch {
    res.json({ text: "" });
  }
});

export default router;
