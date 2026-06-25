import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

/** Root directory for all user-writable CLI state (config, traces, …). */
export const HIVER_DIR = join(homedir(), ".hiver");

/** Persistent CLI config, in a user-writable location. */
export const CONFIG_PATH = join(HIVER_DIR, "config.json");

export interface HiveConfig {
  gatewayUrl?: string;
  /**
   * How the local stack was last brought up: `true` ⇒ `hiver up --microvm`
   * (microVM image variants), `false`/absent ⇒ container variants. `start` reads
   * this to resolve a logical image name to the matching ref in sandbox-images.json.
   */
  microvm?: boolean;
}

export function readConfig(path = CONFIG_PATH): HiveConfig {
  try {
    return JSON.parse(readFileSync(path, "utf8")) as HiveConfig;
  } catch {
    return {}; // missing or unreadable — start fresh
  }
}

/** Merge `patch` into the config file, creating it (and its dir) if needed. */
export function writeConfig(
  patch: Partial<HiveConfig>,
  path = CONFIG_PATH,
): void {
  const merged = { ...readConfig(path), ...patch };
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, JSON.stringify(merged, null, 2) + "\n");
}
