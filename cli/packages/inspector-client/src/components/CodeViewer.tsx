import { lazy, Suspense } from "react";
import type { CodeViewerProps } from "./CodeViewerImpl";

// Monaco (the editor core, ~5 MB) is the single largest dependency in the
// bundle. It is only ever reached through CodeViewer/CodeEditor, so we lazy-load
// the real implementation here: the entry chunk stays small and Monaco is only
// fetched the first time a code surface actually mounts. The fallback is a blank
// placeholder sized to match the editor so there is no layout shift on load.
export const CODE_DIALOG_CLASS = "max-w-4xl";

const CodeViewerImpl = lazy(() => import("./CodeViewerImpl"));

// Keep in sync with CodeViewerImpl's height math so the placeholder reserves the
// same space the editor will occupy.
const LINE_HEIGHT = 19;
const PADDING = 16;

export function CodeViewer(props: CodeViewerProps) {
  return (
    <Suspense fallback={<CodeViewerFallback {...props} />}>
      <CodeViewerImpl {...props} />
    </Suspense>
  );
}

function CodeViewerFallback({
  content,
  className,
  autoSize,
  maxHeight,
  minHeight = 0,
}: CodeViewerProps) {
  const lines = content.split("\n").length;
  const style =
    maxHeight !== undefined
      ? { height: Math.max(Math.min(lines * LINE_HEIGHT + PADDING, maxHeight), minHeight) }
      : autoSize
        ? { height: lines * LINE_HEIGHT + PADDING }
        : undefined;
  return (
    <div
      className={`overflow-hidden bg-zinc-100 dark:bg-zinc-800 ${style ? "" : "h-full"} ${className ?? ""}`}
      style={style}
    />
  );
}
