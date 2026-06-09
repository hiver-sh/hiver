// monaco-editor's package.json `exports` only declares a `types` condition for
// the root entry ("."), so the slim deep ESM imports used in lib/monaco.ts have
// no resolvable type declarations under `bundler` module resolution (and
// edcore.main ships no .d.ts at all). These ambient declarations map the deep
// runtime entry points to the package's root types.
declare module "monaco-editor/esm/vs/editor/edcore.main" {
  export * from "monaco-editor";
}

declare module "monaco-editor/esm/vs/language/json/monaco.contribution" {
  export const jsonDefaults: typeof import("monaco-editor").languages.json.jsonDefaults;
  export function getWorker(): Promise<unknown>;
}

// Grammar contributions are imported for their registration side effects only.
declare module "monaco-editor/esm/vs/basic-languages/*";
