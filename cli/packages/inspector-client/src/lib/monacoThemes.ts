// Registers the app's Monaco themes as a module side effect. Both CodeViewer and
// CodeEditor are lazy-loaded into separate chunks, so the defineTheme calls must
// live in a module they BOTH import — otherwise mounting a CodeEditor without a
// CodeViewer (e.g. the config editor) would reference a theme name Monaco never
// defined and silently fall back to its default, dropping our syntax colors.
import monaco from "@/lib/monaco";
import {
  MONACO_DARK_THEME,
  MONACO_LIGHT_THEME,
  MONACO_DARK_MUTED_THEME,
  MONACO_LIGHT_MUTED_THEME,
} from "@/lib/useMonacoTheme";

const DARK_RULES = [
  { token: "", foreground: "ffffff" },
  { token: "comment", foreground: "71717a", fontStyle: "italic" },
  { token: "keyword", foreground: "ffffff", fontStyle: "bold" },
  { token: "keyword.operator", foreground: "ffffff" },
  { token: "string", foreground: "93c5fd" },
  { token: "number", foreground: "ffffff" },
  { token: "type", foreground: "ffffff" },
  { token: "type.identifier", foreground: "ffffff" },
  { token: "delimiter", foreground: "a1a1aa" },
  { token: "operator", foreground: "ffffff" },
  { token: "identifier", foreground: "ffffff" },
  { token: "variable", foreground: "ffffff" },
  { token: "regexp", foreground: "93c5fd" },
  // HTML / XML
  { token: "tag", foreground: "7dd3fc" },
  { token: "metatag", foreground: "c4b5fd" },
  { token: "attribute.name", foreground: "86efac" },
  { token: "attribute.value", foreground: "93c5fd" },
];

const LIGHT_RULES = [
  { token: "", foreground: "18181b" },
  { token: "comment", foreground: "71717a", fontStyle: "italic" },
  { token: "keyword", foreground: "7c3aed", fontStyle: "bold" },
  { token: "keyword.operator", foreground: "71717a" },
  { token: "string", foreground: "0369a1" },
  { token: "number", foreground: "c2410c" },
  { token: "type", foreground: "18181b" },
  { token: "type.identifier", foreground: "18181b" },
  { token: "delimiter", foreground: "71717a" },
  { token: "operator", foreground: "71717a" },
  { token: "identifier", foreground: "18181b" },
  { token: "variable", foreground: "18181b" },
  { token: "regexp", foreground: "0369a1" },
  // HTML / XML
  { token: "tag", foreground: "0369a1" },
  { token: "metatag", foreground: "7c3aed" },
  { token: "attribute.name", foreground: "166534" },
  { token: "attribute.value", foreground: "c2410c" },
];

monaco.editor.defineTheme(MONACO_DARK_THEME, {
  base: "vs-dark",
  inherit: false,
  rules: DARK_RULES,
  colors: {
    "editor.background": "#1e1e1e",
    "editor.foreground": "#fafafa",
    "editor.lineHighlightBackground": "#18181b",
    "editor.selectionBackground": "#27272a",
    "editor.inactiveSelectionBackground": "#1c1c1f",
    "editorCursor.foreground": "#ffffff",
    "editorLineNumber.foreground": "#27272a",
    "editorIndentGuide.background1": "#18181b",
    "editorWidget.background": "#18181b",
    "editorHoverWidget.background": "#18181b",
    "editorHoverWidget.border": "#27272a",
    "scrollbar.shadow": "#00000000",
    "scrollbarSlider.background": "#27272a66",
    "scrollbarSlider.hoverBackground": "#27272aaa",
  },
});

monaco.editor.defineTheme(MONACO_LIGHT_THEME, {
  base: "vs",
  inherit: false,
  rules: LIGHT_RULES,
  colors: {
    "editor.background": "#ffffff",
    "editor.foreground": "#18181b",
    "editor.lineHighlightBackground": "#f4f4f5",
    "editor.selectionBackground": "#dbeafe",
    "editor.inactiveSelectionBackground": "#e0e7ff",
    "editorCursor.foreground": "#18181b",
    "editorLineNumber.foreground": "#d4d4d8",
    "editorIndentGuide.background1": "#e4e4e7",
    "editorWidget.background": "#f4f4f5",
    "editorHoverWidget.background": "#f4f4f5",
    "editorHoverWidget.border": "#e4e4e7",
    "scrollbar.shadow": "#00000000",
    "scrollbarSlider.background": "#d4d4d866",
    "scrollbarSlider.hoverBackground": "#a1a1aaaa",
  },
});

// Muted variants paint a transparent background so a gray wrapper (e.g. a
// read-only example panel) shows through instead of the editor's own surface.
monaco.editor.defineTheme(MONACO_DARK_MUTED_THEME, {
  base: "vs-dark",
  inherit: false,
  rules: DARK_RULES,
  colors: {
    "editor.background": "#00000000",
    "editor.foreground": "#fafafa",
    "editor.lineHighlightBackground": "#00000000",
    "editor.selectionBackground": "#27272a",
    "editorCursor.foreground": "#ffffff",
    "scrollbar.shadow": "#00000000",
    "scrollbarSlider.background": "#3f3f4666",
    "scrollbarSlider.hoverBackground": "#3f3f46aa",
  },
});

monaco.editor.defineTheme(MONACO_LIGHT_MUTED_THEME, {
  base: "vs",
  inherit: false,
  rules: LIGHT_RULES,
  colors: {
    "editor.background": "#00000000",
    "editor.foreground": "#18181b",
    "editor.lineHighlightBackground": "#00000000",
    "editor.selectionBackground": "#dbeafe",
    "editorCursor.foreground": "#18181b",
    "scrollbar.shadow": "#00000000",
    "scrollbarSlider.background": "#a1a1aa66",
    "scrollbarSlider.hoverBackground": "#a1a1aaaa",
  },
});
