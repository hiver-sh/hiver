import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

/** Root directory for all user-writable CLI state (config, traces, …). */
export const HIVER_DIR = join(homedir(), ".hiver");

/** Persistent CLI config, in a user-writable location. */
export const CONFIG_PATH = join(HIVER_DIR, "config.json");

/** A logical image's source: the Docker ref and whether to pack/prewarm it. */
export interface ImageEntry {
  ref: string;
  /** Per-image pack override; falls back to the file-wide `pack` (default true). */
  pack?: boolean;
}

export interface HiveConfig {
  gatewayUrl?: string;
  /** File-wide default pack mode applied to images that don't set their own. */
  pack?: boolean;
  /** Logical image name → source (design §11). Mounted into the controller. */
  images?: Record<string, ImageEntry>;
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
