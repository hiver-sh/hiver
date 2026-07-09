import { useMemo, useRef, useState } from "react";
import { Check, Clipboard, Maximize2 } from "lucide-react";
import MonacoEditor, { loader } from "@monaco-editor/react";
import type * as Monaco from "monaco-editor";
import monaco from "@/lib/monaco";
import "@/lib/monacoThemes";
import { useMonacoTheme } from "@/lib/useMonacoTheme";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { CODE_DIALOG_CLASS } from "@/components/CodeViewer";

loader.config({ monaco });

const LINE_HEIGHT = 19; // matches fontSize 13 + Monaco default line spacing
const PADDING = 16;

export interface CodeViewerProps {
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
  /** Paints a transparent background so the wrapper's color shows through. */
  muted?: boolean;
}

const LANG_MAP: Record<string, string> = {
  json: "json",
  typescript: "typescript",
  python: "python",
  text: "plaintext",
};

export default function CodeViewer({
  content,
  lang = "text",
  className,
  autoSize,
  maxHeight,
  minHeight = 0,
  expandable,
  muted,
}: CodeViewerProps) {
  const [copied, setCopied] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const monacoTheme = useMonacoTheme(muted);

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
    maxTokenizationLineLength: 400_000,
    // Disable the word highlighter: on unmount it disposes a pending Delayer,
    // rejecting its promise with an uncaught CancellationError (surfaces as a
    // crash in the dev overlay). Occurrence highlighting is useless here anyway.
    occurrencesHighlight: "off",
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
          loading={null}
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
            <DialogTitle className="sr-only">Code viewer</DialogTitle>
            <div className="flex-1 min-h-0 relative">
              <MonacoEditor
                height="100%"
                language={monacoLang}
                value={content}
                theme={monacoTheme}
                loading={null}
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
