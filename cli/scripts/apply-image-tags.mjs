#!/usr/bin/env node
// Bakes the release's immutable per-version image tags into the CLI's bundled
// container-config, so a published CLI (`hiver up` / `hiver start`) references
// the exact sandbox images shipped with the same release instead of `:latest`.
//
// Input is the lock JSON produced by scripts/tag-sandbox-images.sh (path in the
// HIVER_IMAGE_LOCK env var), e.g.
//   { "version": "0.1.31",
//     "images": { "claude": { "image": "hiversh/claude:0.1.31",
//                             "microvm": "hiversh/claude:0.1.31-microvm" }, ... } }
//
// It rewrites, in place (working tree — the Release job packs but does NOT
// commit these):
//   container-config/sandbox-images.json  — each entry's image/microvm refs
//                                           (config/entrypoint left untouched)
//   container-config/compose.yaml         — controller/gateway ${TAG:-latest}
//                                           default -> ${TAG:-<version>}
//
// No-op when HIVER_IMAGE_LOCK is unset, so ordinary dev builds keep `:latest`.

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const CONFIG_DIR = path.resolve(__dirname, "../container-config");
const CATALOG = path.join(CONFIG_DIR, "sandbox-images.json");
const COMPOSE = path.join(CONFIG_DIR, "compose.yaml");

const lockPath = process.env.HIVER_IMAGE_LOCK;
if (!lockPath) {
  console.log("apply-image-tags: HIVER_IMAGE_LOCK unset, leaving :latest tags.");
  process.exit(0);
}

const lock = JSON.parse(await fs.readFile(lockPath, "utf8"));
const { version, images } = lock;
if (!version || !images) {
  console.error(`apply-image-tags: ${lockPath} missing "version"/"images".`);
  process.exit(1);
}

// sandbox-images.json: swap each catalog entry's refs for the locked ones.
const catalog = JSON.parse(await fs.readFile(CATALOG, "utf8"));
for (const [name, entry] of Object.entries(catalog)) {
  const locked = images[name];
  if (!locked) {
    console.error(`apply-image-tags: no lock entry for catalog image "${name}".`);
    process.exit(1);
  }
  entry.image = locked.image;
  entry.microvm = locked.microvm;
}
await fs.writeFile(CATALOG, JSON.stringify(catalog, null, 2) + "\n");

// compose.yaml: the controller/gateway images default to ${TAG:-latest}; pin the
// default to this version so `hiver up` runs the matching control plane.
const compose = await fs.readFile(COMPOSE, "utf8");
const pinned = compose.replaceAll("${TAG:-latest}", `\${TAG:-${version}}`);
await fs.writeFile(COMPOSE, pinned);

console.log(`apply-image-tags: pinned container-config to v${version}.`);
