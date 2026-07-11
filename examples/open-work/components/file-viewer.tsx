"use client";

import * as React from "react";
import { ExternalLink, Loader2, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";
import { SmoothMarkdown } from "@/components/markdown";
import type { OutputFile } from "@/lib/types";

const IMAGE_EXT = ["png", "jpg", "jpeg", "gif", "webp", "svg", "avif", "bmp"];
const BINARY_EXT = ["pdf", "zip", "gz", "tar", "wasm", "bin", "exe", "mp4", "mov", "mp3", "wav"];

export function FileViewer({
  session,
  file,
  gatewayUrl,
  reloadKey = 0,
  onClose,
}: {
  session: string;
  file: OutputFile;
  gatewayUrl: string;
  /** Bumped when the open file is overwritten, to re-fetch its content. */
  reloadKey?: number;
  onClose: () => void;
}) {
  // `reloadKey` busts the URL so a re-fetch (text) or <img> reload (images) gets
  // the new content when the file is overwritten.
  const url = `/api/file?session=${encodeURIComponent(session)}&path=${encodeURIComponent(file.path)}&gatewayUrl=${encodeURIComponent(gatewayUrl)}&v=${reloadKey}`;
  const ext = file.name.split(".").pop()?.toLowerCase() ?? "";
  const isImage = IMAGE_EXT.includes(ext);
  const isMarkdown = ext === "md" || ext === "markdown";
  const isBinary = BINARY_EXT.includes(ext);

  const [text, setText] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(!isImage && !isBinary);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (isImage || isBinary) return;
    let cancelled = false;
    // Fetch in the background and only swap the content in once it's fully
    // loaded — never clear or show a spinner on a live reload (no flash). The
    // spinner shows only on the first load (loading starts true, text is null).
    fetch(url, { cache: "no-store" })
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(`${r.status}`))))
      .then((t) => !cancelled && setText(t))
      .catch(() => !cancelled && setError("Couldn't load this file."))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [url, isImage, isBinary]);

  // Close on Escape.
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <section className="flex h-full min-w-0 flex-1 flex-col border-l bg-background">
      <div className="flex h-14 shrink-0 items-center gap-2 border-b px-4">
        <span className="min-w-0 flex-1 truncate font-mono text-sm">{file.name}</span>
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label="Open in new tab"
          onClick={() => window.open(url, "_blank", "noopener")}
        >
          <ExternalLink />
        </Button>
        <Button variant="ghost" size="icon-sm" aria-label="Close viewer" onClick={onClose}>
          <X />
        </Button>
      </div>

      <ScrollArea className="flex-1 p-5">
        {isImage ? (
          <div className="flex justify-center">
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img src={url} alt={file.name} className="max-w-full rounded-md" />
          </div>
        ) : isBinary ? (
          <p className="text-sm text-muted-foreground">
            No preview for .{ext} files.{" "}
            <button className="underline" onClick={() => window.open(url, "_blank")}>
              Open in new tab
            </button>
            .
          </p>
        ) : text !== null ? (
          // Existing content always wins — a background reload swaps it in only
          // when done, and a failed reload keeps the current content.
          isMarkdown ? (
            <div className="mx-auto max-w-3xl">
              <SmoothMarkdown content={text} streaming={false} />
            </div>
          ) : (
            <pre className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed">
              {text}
            </pre>
          )
        ) : loading ? (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            Loading…
          </div>
        ) : error ? (
          <p className="text-sm text-destructive">{error}</p>
        ) : null}
      </ScrollArea>
    </section>
  );
}
