import { lazy, Suspense } from "react";
import type { CodeEditorProps } from "./CodeEditorImpl";

// See CodeViewer.tsx: Monaco is lazy-loaded so it stays out of the entry chunk
// and is only fetched when an editor first mounts. The fallback is a blank gray
// placeholder sized to match the editor so there is no layout shift on load.
const CodeEditorImpl = lazy(() => import("./CodeEditorImpl"));

const LINE_HEIGHT = 19;
const PADDING = 16;

export function CodeEditor(props: CodeEditorProps) {
  return (
    <Suspense fallback={<CodeEditorFallback {...props} />}>
      <CodeEditorImpl {...props} />
    </Suspense>
  );
}

function CodeEditorFallback({ value, className, autoSize }: CodeEditorProps) {
  const style = autoSize
    ? { height: value.split("\n").length * LINE_HEIGHT + PADDING }
    : undefined;
  return (
    <div
      className={`overflow-hidden rounded-md border border-input bg-zinc-100 dark:bg-zinc-800 ${style ? "" : "h-full"} ${className ?? ""}`}
      style={style}
    />
  );
}
