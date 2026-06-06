import { useMemo, useRef, useState } from "react";
import { Check, Clipboard, Maximize2 } from "lucide-react";
import MonacoEditor, { loader } from "@monaco-editor/react";
import type * as Monaco from "monaco-editor";
import monaco from "@/lib/monaco";
import {
  MONACO_DARK_THEME,
  MONACO_LIGHT_THEME,
  useMonacoTheme,
} from "@/lib/useMonacoTheme";
import { Dialog, DialogContent } from "@/components/ui/dialog";

loader.config({ monaco });

monaco.editor.defineTheme(MONACO_DARK_THEME, {
  base: "vs-dark",
  inherit: false,
  rules: [
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
  ],
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
  rules: [
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
  ],
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

const LINE_HEIGHT = 19; // matches fontSize 13 + Monaco default line spacing
const PADDING = 16;

export const CODE_DIALOG_CLASS = "max-w-4xl";

interface Props {
  content: string;
  lang?: string;
  className?: string;
  /** Sizes the editor to its content height with no internal scrollbar. */
  autoSize?: boolean;
  /** When set, auto-sizes to content between minHeight and maxHeight instead of filling the parent. */
  maxHeight?: number;
  minHeight?: number;
  /** Shows an expand button that opens the editor in a full-screen dialog. */
  expandable?: boolean;
}

const LANG_MAP: Record<string, string> = {
  json: "json",
  typescript: "typescript",
  python: "python",
  text: "plaintext",
};

export function CodeViewer({
  content,
  lang = "text",
  className,
  autoSize,
  maxHeight,
  minHeight = 0,
  expandable,
}: Props) {
  const [copied, setCopied] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const monacoTheme = useMonacoTheme();

  const height = useMemo(() => {
    if (autoSize) return "100%";
    if (maxHeight === undefined) return "100%";
    const lines = content.split("\n").length;
    return Math.max(
      Math.min(lines * LINE_HEIGHT + PADDING, maxHeight),
      minHeight,
    );
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

  const editorOptions: Monaco.editor.IStandaloneEditorConstructionOptions = {
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
    scrollbar: {
      alwaysConsumeMouseWheel: false,
      verticalScrollbarSize: 10,
      horizontalScrollbarSize: 10,
    },
    inlayHints: { enabled: "off" },
  };

  const monacoLang = LANG_MAP[lang ?? ""] ?? lang ?? "plaintext";

  return (
    <>
      <div
        ref={containerRef}
        className={`relative overflow-hidden h-full ${className ?? ""}`}
      >
        <MonacoEditor
          height={height}
          onMount={handleMount}
          language={monacoLang}
          value={content}
          theme={monacoTheme}
          options={editorOptions}
        />
        <div className="absolute top-2 right-3 flex gap-1 z-10">
          {expandable && (
            <button
              onClick={() => setExpanded(true)}
              className="p-1.5 rounded-md bg-muted/60 hover:bg-muted text-muted-foreground hover:text-foreground transition-colors"
              title="Expand"
            >
              <Maximize2 className="h-3.5 w-3.5" />
            </button>
          )}
          <button
            onClick={handleCopy}
            className="p-1.5 rounded-md bg-muted/60 hover:bg-muted text-muted-foreground hover:text-foreground transition-colors"
            title="Copy to clipboard"
          >
            {copied ? (
              <Check className="h-3.5 w-3.5" />
            ) : (
              <Clipboard className="h-3.5 w-3.5" />
            )}
          </button>
        </div>
      </div>

      {expandable && (
        <Dialog open={expanded} onOpenChange={setExpanded}>
          <DialogContent
            className={`${CODE_DIALOG_CLASS} p-0 flex flex-col overflow-hidden h-[55vh]`}
          >
            <div className="flex-1 min-h-0 relative">
              <MonacoEditor
                height="100%"
                language={monacoLang}
                value={content}
                theme={monacoTheme}
                options={{
                  ...editorOptions,
                  scrollbar: {
                    verticalScrollbarSize: 10,
                    horizontalScrollbarSize: 10,
                  },
                }}
              />
            </div>
          </DialogContent>
        </Dialog>
      )}
    </>
  );
}
