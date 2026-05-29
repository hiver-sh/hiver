import { Sandbox } from "hive";
import type { Request } from "express";
import { DEFAULT_URL } from "./controllerUrl.js";

export function sandboxFromReq(req: Request): Sandbox | null {
  const url = req.query.sandboxUrl as string | undefined;
  if (!url) return null;
  return new Sandbox({ id: req.params.id, endpoint: url }, { controllerUrl: DEFAULT_URL });
}
