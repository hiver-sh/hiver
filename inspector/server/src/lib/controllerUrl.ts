import type { Request } from "express";

export const DEFAULT_URL = process.env.GATEWAY_URL ?? "http://localhost:10000";

export function gatewayUrl(req: Request): string {
  const override =
    (req.query.gateway as string | undefined) ??
    req.headers["x-gateway-url"];
  return typeof override === "string" && override.length > 0
    ? override
    : DEFAULT_URL;
}
