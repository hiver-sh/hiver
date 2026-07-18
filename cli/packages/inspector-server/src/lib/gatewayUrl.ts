import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { Request } from "express";

const FALLBACK_URL = "http://localhost:10000";

// The gateway `hiver connect` last saved, in the CLI's user-writable config.
// (Kept in sync with commands/src/config.ts — the server can't import from the
// commands package.)
const CONFIG_PATH = join(homedir(), ".hiver", "config.json");

/**
 * The server's default gateway, resolved live per call (not frozen at startup):
 * an explicit `--gateway-url` flag pinned by `hiver inspect` via GATEWAY_URL
 * wins, else the value `hiver connect` saved to config, else the built-in
 * default. Reading config each time means a reused devtools server — one
 * `hiver inspect` left running while you `hiver connect` elsewhere — reflects
 * the new gateway without a restart.
 */
export function defaultGatewayUrl(): string {
  if (process.env.GATEWAY_URL) return process.env.GATEWAY_URL;
  try {
    const cfg = JSON.parse(readFileSync(CONFIG_PATH, "utf8")) as {
      gatewayUrl?: string;
    };
    if (cfg.gatewayUrl) return cfg.gatewayUrl;
  } catch {
    /* missing or unreadable config — fall through to the default */
  }
  return FALLBACK_URL;
}

export function gatewayUrl(req: Request): string {
  const override =
    (req.query.gateway as string | undefined) ?? req.headers["x-gateway-url"];
  return typeof override === "string" && override.length > 0
    ? override
    : defaultGatewayUrl();
}
