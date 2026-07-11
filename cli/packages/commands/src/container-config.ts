import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import type { SandboxConfig } from "@hiver.sh/client";

const configDir = resolve(dirname(fileURLToPath(import.meta.url)), "../../../container-config");

export const composePath = resolve(configDir, "compose.yaml");

/**
 * A catalog entry (sandbox-images.json): the container `image` ref, the
 * `microvm` ref to use instead when the stack runs the microVM variant, and
 * `config` — the `SandboxConfig` launch defaults for the image (e.g. `tty`,
 * `cwd`; `{}` when there are none).
 */
interface ImageEntry {
  image: string;
  microvm: string;
  config: SandboxConfig;
}

const catalog: Record<string, ImageEntry> = JSON.parse(
  readFileSync(resolve(configDir, "sandbox-images.json"), "utf8"),
);

// logical name → ref maps for each variant, derived from the catalog. `up` reads
// these to build the controller's image config (HIVER_IMAGES_CONFIG).
export const sandboxImages: Record<string, string> = Object.fromEntries(
  Object.entries(catalog).map(([name, e]) => [name, e.image]),
);
export const sandboxImagesMicrovm: Record<string, string> = Object.fromEntries(
  Object.entries(catalog).map(([name, e]) => [name, e.microvm]),
);

/**
 * Resolve a logical image name (e.g. `browser`, `claude`) to its concrete
 * Docker ref from sandbox-images.json, picking the variant that matches how the
 * local stack was brought up (`microvm` ⇒ the `-microvm` ref). Returns
 * `undefined` for names not in the catalog — i.e. raw refs or Dockerfile dirs,
 * which the caller should pass through untouched.
 */
export function resolveSandboxImage(
  name: string,
  microvm: boolean,
): string | undefined {
  const entry = catalog[name];
  if (!entry) return undefined;
  return microvm ? entry.microvm : entry.image;
}

/**
 * The launch defaults (`SandboxConfig`) declared for a logical image — e.g.
 * `tty`/`cwd`. The caller resolves the image ref separately via
 * {@link resolveSandboxImage}. Empty for names not in the catalog, so raw refs
 * and Dockerfile dirs contribute no defaults.
 */
export function imageConfig(name: string): SandboxConfig {
  return catalog[name]?.config ?? {};
}
