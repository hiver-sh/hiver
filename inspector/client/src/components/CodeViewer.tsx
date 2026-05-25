import { useMemo, useRef, useState } from "react";
import { Check, Clipboard } from "lucide-react";
import MonacoEditor, { loader } from "@monaco-editor/react";
import type * as Monaco from "monaco-editor";
import * as monaco from "monaco-editor";
import { MONACO_THEME } from "@/monacoWorkers";

loader.config({ monaco });

monaco.editor.defineTheme("hive-dark", {
  base: "vs-dark",
  inherit: false,
  rules: [
    { token: "",                  foreground: "ffffff" },
    { token: "comment",           foreground: "71717a", fontStyle: "italic" },
    { token: "keyword",           foreground: "ffffff", fontStyle: "bold" },
    { token: "keyword.operator",  foreground: "ffffff" },
    { token: "string",            foreground: "93c5fd" },
    { token: "number",            foreground: "ffffff" },
    { token: "type",              foreground: "ffffff" },
    { token: "type.identifier",   foreground: "ffffff" },
    { token: "delimiter",         foreground: "a1a1aa" },
    { token: "operator",          foreground: "ffffff" },
    { token: "identifier",        foreground: "ffffff" },
    { token: "variable",          foreground: "ffffff" },
    { token: "regexp",            foreground: "93c5fd" },
  ],
  colors: {
    "editor.background":                  "#1e1e1e",
    "editor.foreground":                  "#fafafa",
    "editor.lineHighlightBackground":     "#18181b",
    "editor.selectionBackground":         "#27272a",
    "editor.inactiveSelectionBackground": "#1c1c1f",
    "editorCursor.foreground":            "#ffffff",
    "editorLineNumber.foreground":        "#27272a",
    "editorIndentGuide.background1":      "#18181b",
    "editorWidget.background":            "#18181b",
    "editorHoverWidget.background":       "#18181b",
    "editorHoverWidget.border":           "#27272a",
    "scrollbar.shadow":                   "#00000000",
    "scrollbarSlider.background":         "#27272a66",
    "scrollbarSlider.hoverBackground":    "#27272aaa",
  },
});

const LINE_HEIGHT = 19; // matches fontSize 13 + Monaco default line spacing
const PADDING = 16;

interface Props {
  content: string;
  lang?: "json" | "text" | "typescript" | "python";
  className?: string;
  /** Sizes the editor to its content height with no internal scrollbar. */
  autoSize?: boolean;
  /** When set, auto-sizes to content between minHeight and maxHeight instead of filling the parent. */
  maxHeight?: number;
  minHeight?: number;
}

const LANG_MAP: Record<string, string> = {
  json: "json",
  typescript: "typescript",
  python: "python",
  text: "plaintext",
};

export function CodeViewer({ content, lang = "text", className, autoSize, maxHeight, minHeight = 0 }: Props) {
  const [copied, setCopied] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  const height = useMemo(() => {
    if (autoSize) return "100%";
    if (maxHeight === undefined) return "100%";
    const lines = content.split("\n").length;
    return Math.max(Math.min(lines * LINE_HEIGHT + PADDING, maxHeight), minHeight);
  }, [autoSize, content, maxHeight, minHeight]);

  function handleMount(editor: Monaco.editor.IStandaloneCodeEditor) {
    if (!autoSize) return;
    const update = () => {
      const h = editor.getContentHeight();
      if (containerRef.current) containerRef.current.style.height = `${h}px`;
      editor.layout();
    };
    editor.onDidContentSizeChange(update);
    update();
  }

  function handleCopy() {
    navigator.clipboard.writeText(content);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <div ref={containerRef} className={`relative overflow-hidden h-full ${className ?? ""}`}>
      <MonacoEditor
        height={height}
        onMount={handleMount}
        language={LANG_MAP[lang] ?? "plaintext"}
        value={content}
        theme={MONACO_THEME}
        options={{
          readOnly: true,
          minimap: { enabled: false },
          lineNumbers: "off",
          scrollBeyondLastLine: false,
          wordWrap: "on",
          tabSize: 2,
          automaticLayout: true,
          fontSize: 13,
          padding: { top: 8, bottom: 8 },
          folding: false,
          scrollbar: { alwaysConsumeMouseWheel: false },
        }}
      />
      <button
        onClick={handleCopy}
        className="absolute top-2 right-3 p-1.5 rounded-md bg-muted/60 hover:bg-muted text-muted-foreground hover:text-foreground transition-colors z-10"
        title="Copy to clipboard"
      >
        {copied ? <Check className="h-3.5 w-3.5" /> : <Clipboard className="h-3.5 w-3.5" />}
      </button>
    </div>
  );
}
