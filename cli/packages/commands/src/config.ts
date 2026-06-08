import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

/** Root directory for all user-writable CLI state (config, traces, …). */
export const HIVER_DIR = join(homedir(), ".hiver");

/** Persistent CLI config, in a user-writable location. */
export const CONFIG_PATH = join(HIVER_DIR, "config.json");

export interface HiveConfig {
  gatewayUrl?: string;
}

export function readConfig(): HiveConfig {
  try {
    return JSON.parse(readFileSync(CONFIG_PATH, "utf8")) as HiveConfig;
  } catch {
    return {}; // missing or unreadable — start fresh
  }
}

/** Merge `patch` into the config file, creating it (and its dir) if needed. */
export function writeConfig(patch: Partial<HiveConfig>): void {
  const merged = { ...readConfig(), ...patch };
  mkdirSync(dirname(CONFIG_PATH), { recursive: true });
  writeFileSync(CONFIG_PATH, JSON.stringify(merged, null, 2) + "\n");
}
