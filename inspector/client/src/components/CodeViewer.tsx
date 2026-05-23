import { useEffect, useRef, useState } from "react";
import { Check, Clipboard } from "lucide-react";
import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { basicSetup } from "codemirror";
import { json } from "@codemirror/lang-json";
import { vscodeDark } from "./editorTheme";

interface Props {
  content: string;
  lang?: "json" | "text";
  className?: string;
}

export function CodeViewer({ content, lang = "text", className }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const viewRef      = useRef<EditorView | null>(null);
  const [copied, setCopied] = useState(false);

  function handleCopy() {
    navigator.clipboard.writeText(viewRef.current?.state.doc.toString() ?? content);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    const extensions = [
      basicSetup,
      ...vscodeDark,
      json(),
      EditorView.editable.of(false),
      EditorState.readOnly.of(true),
      EditorView.lineWrapping,
    ];

    viewRef.current = new EditorView({
      state: EditorState.create({ doc: content, extensions }),
      parent: el,
    });

    return () => { viewRef.current?.destroy(); viewRef.current = null; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lang]);

  // Patch content without recreating the editor; append-only when possible.
  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    const cur = view.state.doc.toString();
    if (cur === content) return;
    if (content.startsWith(cur)) {
      view.dispatch({ changes: { from: cur.length, insert: content.slice(cur.length) } });
    } else {
      view.dispatch({ changes: { from: 0, to: cur.length, insert: content } });
    }
  }, [content]);

  return (
    <div className={`relative h-full overflow-hidden ${className ?? ""}`}>
      <div ref={containerRef} className="h-full" />
      <button
        onClick={handleCopy}
        className="absolute top-2 right-3 p-1.5 rounded-md bg-muted/60 hover:bg-muted text-muted-foreground hover:text-foreground transition-colors"
        title="Copy to clipboard"
      >
        {copied ? <Check className="h-3.5 w-3.5" /> : <Clipboard className="h-3.5 w-3.5" />}
      </button>
    </div>
  );
}
