"use client";

import * as React from "react";
import remarkBreaks from "remark-breaks";
import { Streamdown, defaultRemarkPlugins } from "streamdown";

import { cn } from "@/lib/utils";

// GFM defaults + remark-breaks so a single newline renders as a line break
// (otherwise markdown collapses single newlines into soft breaks).
const remarkPlugins = [...Object.values(defaultRemarkPlugins), remarkBreaks];

/**
 * Renders streaming markdown with a buttery reveal. `content` is the full text
 * received so far; we ease a "shown" cursor toward its end one animation frame
 * at a time, so bursty SSE chunks turn into a steady, smooth stream. Streamdown
 * tolerates the half-finished markdown we slice off mid-token.
 */
export function SmoothMarkdown({
  content,
  streaming,
  className,
}: {
  content: string;
  streaming: boolean;
  className?: string;
}) {
  // Restored (already-complete) turns render in full immediately; only a live
  // streaming turn animates the reveal.
  const [shown, setShown] = React.useState(() =>
    streaming ? 0 : content.length,
  );

  React.useEffect(() => {
    // Not streaming → snap to the full text, no animation.
    if (!streaming) {
      if (shown !== content.length) setShown(content.length);
      return;
    }
    if (shown >= content.length) return;
    const id = requestAnimationFrame(() => {
      const remaining = content.length - shown;
      // Ease-out: reveal a fraction of what's left, min 2 chars/frame.
      const step = Math.max(2, Math.ceil(remaining * 0.2));
      setShown((s) => Math.min(content.length, s + step));
    });
    return () => cancelAnimationFrame(id);
  }, [shown, content.length, streaming]);

  const caughtUp = shown >= content.length;
  const text = content.slice(0, shown);
  const showCaret = streaming || !caughtUp;

  return (
    <div className={cn("stream-prose", className)}>
      <Streamdown
        parseIncompleteMarkdown
        remarkPlugins={remarkPlugins}
        // While streaming, each new top-level block fades in as it mounts.
        // Restored turns render without animation.
        className={cn(
          "[&>*:first-child]:mt-0 [&>*:last-child]:mb-0",
          streaming && "[&>*]:animate-fade-in-up",
        )}
      >
        {text}
      </Streamdown>
      {showCaret && <span className="stream-caret align-text-bottom" />}
    </div>
  );
}
