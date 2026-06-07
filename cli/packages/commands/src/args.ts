import arg from "arg";
import { DEFAULT_GATEWAY_URL } from "@hiver.sh/client";
import { readConfig } from "./config.js";

// Flags accepted by every command.
const GLOBAL = {
  "--gateway-url": String,
} as const;

/**
 * Parse a command's args with the global flags merged in. Defaults to argv
 * after the command name. `permissive` keeps unknown flags/positionals in `_`
 * so commands can forward them (e.g. `hiver up --build`).
 */
export function parseArgs<S extends arg.Spec>(spec: S, argv = process.argv.slice(3)) {
  return arg({ ...GLOBAL, ...spec }, { argv, permissive: true });
}

/**
 * The gateway URL for any command: the global `--gateway-url` override, then the
 * URL `hiver up` saved (~/.hive/config.json), then the default. Scans the whole
 * argv so it works regardless of command position.
 */
export function resolveGatewayUrl(): string {
  const parsed = arg(GLOBAL, { argv: process.argv.slice(2), permissive: true });
  return parsed["--gateway-url"] ?? readConfig().gatewayUrl ?? DEFAULT_GATEWAY_URL;
}
