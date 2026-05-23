import { EditorView } from "@codemirror/view";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { tags as t } from "@lezer/highlight";

export const vscodeDark = [
  EditorView.theme(
    {
      "&": { color: "#d4d4d4", backgroundColor: "#18181b", height: "100%", fontSize: "13px" },
      ".cm-content": { caretColor: "#aeafad" },
      ".cm-cursor": { borderLeftColor: "#aeafad" },
      "&.cm-focused .cm-selectionBackground": { backgroundColor: "#264f78 !important" },
      ".cm-selectionBackground": { backgroundColor: "#264f78 !important" },
      "& ::selection": { backgroundColor: "#264f78 !important" },
      ".cm-panels": { backgroundColor: "#18181b", color: "#d4d4d4" },
      ".cm-gutters": { backgroundColor: "#18181b", color: "#858585", border: "none" },
      ".cm-activeLineGutter": { backgroundColor: "#232327" },
      ".cm-activeLine": { backgroundColor: "transparent" },
      ".cm-scroller": { overflow: "auto", fontFamily: '"Google Sans Code", monospace' },
      ".cm-scroller::-webkit-scrollbar": { width: "8px", height: "8px" },
      ".cm-scroller::-webkit-scrollbar-track": { background: "#18181b" },
      ".cm-scroller::-webkit-scrollbar-thumb": { background: "#3f3f46", borderRadius: "4px" },
      ".cm-scroller::-webkit-scrollbar-thumb:hover": { background: "#52525b" },
    },
    { dark: true },
  ),
  syntaxHighlighting(
    HighlightStyle.define([
      { tag: t.keyword, color: "#569cd6" },
      { tag: [t.name, t.deleted, t.character, t.propertyName, t.macroName], color: "#9cdcfe" },
      { tag: [t.function(t.variableName), t.labelName], color: "#dcdcaa" },
      { tag: [t.color, t.constant(t.name), t.standard(t.name)], color: "#569cd6" },
      { tag: [t.definition(t.name), t.separator], color: "#d4d4d4" },
      { tag: [t.typeName, t.className, t.number, t.changed, t.annotation, t.modifier, t.self, t.namespace], color: "#4ec9b0" },
      { tag: [t.operator, t.operatorKeyword, t.url, t.escape, t.regexp, t.link, t.special(t.string)], color: "#d4d4d4" },
      { tag: [t.meta, t.comment], color: "#6a9955" },
      { tag: t.strong, fontWeight: "bold" },
      { tag: t.emphasis, fontStyle: "italic" },
      { tag: t.strikethrough, textDecoration: "line-through" },
      { tag: t.link, color: "#6a9955", textDecoration: "underline" },
      { tag: t.heading, fontWeight: "bold", color: "#569cd6" },
      { tag: [t.atom, t.bool, t.special(t.variableName)], color: "#569cd6" },
      { tag: [t.processingInstruction, t.string, t.inserted], color: "#ce9178" },
      { tag: t.invalid, color: "#f44747" },
    ]),
  ),
];
