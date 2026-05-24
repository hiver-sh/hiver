import { useMemo, useState } from "react";
import { Check, Clipboard } from "lucide-react";
import MonacoEditor, { loader } from "@monaco-editor/react";
import * as monaco from "monaco-editor";

loader.config({ monaco });

const LINE_HEIGHT = 19; // matches fontSize 13 + Monaco default line spacing
const PADDING = 16;

interface Props {
  content: string;
  lang?: "json" | "text";
  className?: string;
  /** When set, auto-sizes to content between minHeight and maxHeight instead of filling the parent. */
  maxHeight?: number;
  minHeight?: number;
}

export function CodeViewer({ content, lang = "text", className, maxHeight, minHeight = 0 }: Props) {
  const [copied, setCopied] = useState(false);

  const height = useMemo(() => {
    if (maxHeight === undefined) return "100%";
    const lines = content.split("\n").length;
    return Math.max(Math.min(lines * LINE_HEIGHT + PADDING, maxHeight), minHeight);
  }, [content, maxHeight, minHeight]);

  function handleCopy() {
    navigator.clipboard.writeText(content);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <div className={`relative overflow-hidden ${className ?? ""}`}>
      <MonacoEditor
        height={height}
        language={lang === "json" ? "json" : "plaintext"}
        value={content}
        theme="vs-dark"
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
