import { Sandbox } from "@hiver.sh/client";
import type { Request } from "express";
import { gatewayUrl } from "./gatewayUrl.js";

export function sandboxFromReq(req: Request): Sandbox {
  // Per-sandbox data-plane routes carry the sandbox id in the path; the gateway
  // routes by id, so the caller-chosen key is unknown here and unused.
  return new Sandbox(
    { id: req.params.id, key: "" },
    { gatewayUrl: gatewayUrl(req) },
  );
}
