import { Sandbox } from "@hiver.sh/client";
import type { Request } from "express";
import { gatewayUrl } from "./gatewayUrl.js";

export function sandboxFromReq(req: Request): Sandbox {
  // Per-sandbox routes only ever carry the key in the path; the uuid is
  // never known here and is unused for routing, so leave it empty.
  return new Sandbox(
    { id: "", key: req.params.key },
    { gatewayUrl: gatewayUrl(req) },
  );
}
