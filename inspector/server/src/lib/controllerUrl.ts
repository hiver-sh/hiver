import { DEFAULT_CONTROLLER_URL } from "hive";
import type { Request } from "express";

export const DEFAULT_URL = process.env.CONTROLLER_URL ?? DEFAULT_CONTROLLER_URL;

export function controllerUrl(req: Request): string {
  const override =
    (req.query.controller as string | undefined) ??
    req.headers["x-controller-url"];
  return typeof override === "string" && override.length > 0
    ? override
    : DEFAULT_URL;
}
