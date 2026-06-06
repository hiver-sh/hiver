#!/usr/bin/env tsx
// Swaps the @hiver.sh/client dependency spec in package.json.
//
// npm cannot link a non-workspace sibling package locally AND publish it as a
// registry version from one field, so we keep the local "file:" link committed
// for development and rewrite it to the published semver range at pack time:
//   prepack  -> ^0.1.0                  (what consumers of @hiver.sh/cli get)
//   postpack -> file:../client/typescript (restored for local dev)
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const spec = process.argv[2];
if (!spec) {
  console.error("usage: client-dep.ts <spec>");
  process.exit(1);
}

const pkgPath = resolve(dirname(fileURLToPath(import.meta.url)), "..", "package.json");
const pkg = JSON.parse(readFileSync(pkgPath, "utf8")) as {
  dependencies: Record<string, string>;
};
pkg.dependencies["@hiver.sh/client"] = spec;
writeFileSync(pkgPath, JSON.stringify(pkg, null, 2) + "\n");
console.log(`set @hiver.sh/client -> ${spec}`);
