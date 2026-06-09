// Slim Monaco build. The full `monaco-editor` barrel registers every language
// service (the ~7 MB ts.worker, css/html workers) plus ~90 grammars, which
// dominated the published bundle. We instead import the core editor (all editor
// features, no languages) and add back only what the app uses:
//   - JSON language service — the config editor needs schema validation (json.worker)
//   - Grammars (highlighting only, no workers): markdown, typescript, python, go
// Everything else falls back to plaintext.
import * as monaco from "monaco-editor/esm/vs/editor/edcore.main";
import "monaco-editor/esm/vs/basic-languages/markdown/markdown.contribution";
import "monaco-editor/esm/vs/basic-languages/typescript/typescript.contribution";
import "monaco-editor/esm/vs/basic-languages/python/python.contribution";
import "monaco-editor/esm/vs/basic-languages/go/go.contribution";

// The json contribution registers the "json" language and owns `jsonDefaults`.
// In the ESM build it does NOT attach `monaco.languages.json` (that namespace
// only exists in the AMD/barrel build), so we re-export `jsonDefaults` directly
// for schema configuration rather than reaching for `monaco.languages.json`.
export { jsonDefaults } from "monaco-editor/esm/vs/language/json/monaco.contribution";

// The json language's tokenizer is wired up lazily: the contribution registers
// it via languages.onLanguage("json", …) which only fires once the first json
// model is created, and the setup itself dynamically imports `jsonMode` (a
// separate chunk). So the first JSON editor would paint as uncolored plaintext
// until that chunk loaded. Eagerly create+dispose a throwaway json model here so
// onLanguage fires at module load and the tokenizer is ready before any editor
// mounts. (The old `monaco-editor` barrel never showed this because jsonMode was
// part of the main bundle.)
monaco.editor.createModel("", "json").dispose();

export default monaco;
