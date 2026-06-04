import { Sandbox } from "hive";
import type { Request } from "express";
import { gatewayUrl } from "./controllerUrl.js";

export function sandboxFromReq(req: Request): Sandbox {
  return new Sandbox({ id: req.params.id }, { gatewayUrl: gatewayUrl(req) });
}
