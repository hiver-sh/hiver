import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const configDir = resolve(dirname(fileURLToPath(import.meta.url)), "../../../container-config");

export const composePath = resolve(configDir, "compose.yaml");

const allSandboxImages: { container: Record<string, string>; microvm: Record<string, string> } = JSON.parse(
  readFileSync(resolve(configDir, "sandbox-images.json"), "utf8"),
);

export const sandboxImages = allSandboxImages.container;
export const sandboxImagesMicrovm = allSandboxImages.microvm;
