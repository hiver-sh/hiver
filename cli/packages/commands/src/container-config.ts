import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const configDir = resolve(dirname(fileURLToPath(import.meta.url)), "../../../container-config");

export const composePath = resolve(configDir, "compose.yaml");

export const sandboxImages: Record<string, string> = JSON.parse(
  readFileSync(resolve(configDir, "sandbox-images.json"), "utf8"),
);
