import { Sandbox } from "@hiver.sh/client";
import type { Request } from "express";
import { gatewayUrl } from "./gatewayUrl.js";

export function sandboxFromReq(req: Request): Sandbox {
  // Per-sandbox data-plane routes carry BOTH the id and the key in the path
  // (/:id/:key/...): the gateway routes to the pod by id, then sandboxd resolves
  // the addressed sandbox by key (/v1/<key>/...). Both are required.
  return new Sandbox(
    { id: req.params.id, key: req.params.key ?? "" },
    { gatewayUrl: gatewayUrl(req) },
  );
}
