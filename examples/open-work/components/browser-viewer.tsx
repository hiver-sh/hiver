"use client";

import * as React from "react";
import { Globe, Loader2, Maximize2, Minus } from "lucide-react";

import { Button } from "@/components/ui/button";

export type BrowserFrame = { data: string; width?: number; height?: number };
export type BrowserMeta = { title: string; url: string; favicon: string };
type Status = "connecting" | "live" | "unavailable" | "error";

/** Just the host (dropping the `www.` prefix), for the compact URL display. */
function hostOf(url: string): string {
  try {
    return new URL(url).hostname.replace(/^www\./, "");
  } catch {
    return url;
  }
}

/** The page favicon, falling back to a globe when it's missing or fails to load. */
function Favicon({ src, className }: { src?: string; className?: string }) {
  const [failed, setFailed] = React.useState(false);
  React.useEffect(() => setFailed(false), [src]);
  if (!src || failed) return <Globe className={className} />;
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img src={src} alt="" className={className} onError={() => setFailed(true)} />
  );
}

/** Compact host label that swaps in place for a read-only, copyable full-URL
 * input on hover (no floating tooltip). Uses explicit hover state rather than
 * CSS group-hover, which is unreliable under the ~25fps screencast re-renders.
 * Host and input share identical box metrics so the swap causes no layout shift. */
function UrlBadge({ url }: { url: string }) {
  const [open, setOpen] = React.useState(false);
  if (!url) return null;
  return (
    <span
      className="relative -ml-1.5 block max-w-full"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
    >
      {/* The host always occupies the layout; the input overlays it absolutely
       * on hover, so revealing the full URL never shifts anything. */}
      <span className="block truncate rounded px-1.5 py-0.5 text-xs text-muted-foreground">
        {hostOf(url)}
      </span>
      {open && (
        <input
          readOnly
          value={url}
          onMouseDown={(e) => e.stopPropagation()}
          onClick={(e) => {
            e.stopPropagation();
            e.currentTarget.select();
          }}
          onFocus={(e) => e.currentTarget.select()}
          className="absolute inset-0 w-full rounded bg-background px-1.5 py-0.5 text-xs text-muted-foreground outline-none ring-1 ring-border"
        />
      )}
    </span>
  );
}

const BUTTON: Record<number, "left" | "middle" | "right"> = {
  0: "left",
  1: "middle",
  2: "right",
};

/**
 * Live screencast + page metadata for a task's browser VM. Kept in the parent so
 * it survives minimizing the viewer to a thumbnail (the stream stays open while
 * `enabled`, so the thumbnail and its URL stay current).
 */
export function useBrowserStream(
  session: string | null,
  gatewayUrl: string,
  enabled: boolean,
) {
  const [frame, setFrame] = React.useState<BrowserFrame | null>(null);
  const [meta, setMeta] = React.useState<BrowserMeta | null>(null);
  const [status, setStatus] = React.useState<Status>("connecting");

  React.useEffect(() => {
    if (!session || !enabled) return;
    const src = `/api/browser/screen?session=${encodeURIComponent(session)}&gatewayUrl=${encodeURIComponent(gatewayUrl)}`;
    const es = new EventSource(src);
    es.addEventListener("frame", (e) => {
      setFrame(JSON.parse((e as MessageEvent).data));
      setStatus("live");
    });
    es.addEventListener("meta", (e) => setMeta(JSON.parse((e as MessageEvent).data)));
    es.addEventListener("unavailable", () => setStatus("unavailable"));
    es.addEventListener("error", () => setStatus("error"));
    es.onerror = () => setStatus((s) => (s === "live" ? s : "error"));
    return () => es.close();
  }, [session, gatewayUrl, enabled]);

  return { frame, meta, status };
}

/** Full interactive viewer column. Mouse/keyboard map back to CDP Input events. */
export function BrowserViewer({
  session,
  gatewayUrl,
  frame,
  meta,
  status,
  onMinimize,
}: {
  session: string;
  gatewayUrl: string;
  frame: BrowserFrame | null;
  meta: BrowserMeta | null;
  status: Status;
  onMinimize: () => void;
}) {
  const imgRef = React.useRef<HTMLImageElement>(null);
  const lastMove = React.useRef(0);

  const post = React.useCallback(
    (event: unknown) =>
      fetch("/api/browser/input", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ session, gatewayUrl, event }),
      }),
    [session, gatewayUrl],
  );
  const send = React.useCallback(
    (event: unknown) => void post(event).catch(() => {}),
    [post],
  );
  const sendForResult = React.useCallback(
    async (event: unknown): Promise<{ ok: boolean; text?: string } | null> => {
      try {
        return await (await post(event)).json();
      } catch {
        return null;
      }
    },
    [post],
  );

  const toFrameCoords = (e: React.MouseEvent) => {
    const img = imgRef.current;
    if (!img) return null;
    const rect = img.getBoundingClientRect();
    if (!rect.width || !rect.height) return null;
    const w = frame?.width ?? img.naturalWidth;
    const h = frame?.height ?? img.naturalHeight;
    return {
      x: ((e.clientX - rect.left) / rect.width) * w,
      y: ((e.clientY - rect.top) / rect.height) * h,
    };
  };

  const onMouseMove = (e: React.MouseEvent) => {
    const now = Date.now();
    if (now - lastMove.current < 40) return; // throttle ~25fps
    lastMove.current = now;
    const c = toFrameCoords(e);
    if (c) send({ kind: "move", ...c });
  };

  const onMouseDown = (e: React.MouseEvent) => {
    const c = toFrameCoords(e);
    if (c) send({ kind: "down", ...c, button: BUTTON[e.button] ?? "left" });
  };

  const onMouseUp = (e: React.MouseEvent) => {
    const c = toFrameCoords(e);
    if (c) send({ kind: "up", ...c, button: BUTTON[e.button] ?? "left" });
  };

  const onWheel = (e: React.WheelEvent) => {
    const c = toFrameCoords(e);
    if (c) send({ kind: "wheel", ...c, dx: e.deltaX, dy: e.deltaY });
  };

  // CDP modifier bitmask: Alt=1, Ctrl=2, Meta=4, Shift=8.
  const modifiers = (e: React.KeyboardEvent) =>
    (e.altKey ? 1 : 0) |
    (e.ctrlKey ? 2 : 0) |
    (e.metaKey ? 4 : 0) |
    (e.shiftKey ? 8 : 0);

  const forwardKey = (down: boolean, e: React.KeyboardEvent) => {
    // Only printable keys (no Cmd/Ctrl held) carry text; Enter inserts a newline.
    const text =
      down && e.key.length === 1 && !e.metaKey && !e.ctrlKey
        ? e.key
        : down && e.key === "Enter"
          ? "\r"
          : undefined;
    send({
      kind: "key",
      down,
      key: e.key,
      code: e.code,
      keyCode: e.keyCode,
      modifiers: modifiers(e),
      text,
    });
  };

  const onKeyDown = async (e: React.KeyboardEvent) => {
    e.preventDefault();
    // Bridge clipboard shortcuts: the remote page can't see the local clipboard.
    const mod = e.metaKey || e.ctrlKey;
    const k = e.key.toLowerCase();
    if (mod && (k === "c" || k === "x")) {
      const res = await sendForResult({ kind: "copy", cut: k === "x" });
      if (res?.text) {
        try {
          await navigator.clipboard.writeText(res.text);
        } catch {
          /* clipboard write blocked */
        }
      }
      return;
    }
    if (mod && k === "v") {
      let text = "";
      try {
        text = await navigator.clipboard.readText();
      } catch {
        /* clipboard read blocked */
      }
      if (text) send({ kind: "paste", text });
      return;
    }
    forwardKey(true, e);
  };

  const onKeyUp = (e: React.KeyboardEvent) => {
    e.preventDefault();
    forwardKey(false, e);
  };

  return (
    <section className="flex h-full min-w-0 flex-1 flex-col border-l bg-background">
      <div className="flex h-14 shrink-0 items-center gap-2.5 border-b px-4">
        <div className="grid min-w-0 flex-1 grid-cols-[auto_1fr] items-center gap-x-2 leading-tight">
          <Favicon src={meta?.favicon} className="size-4 shrink-0 rounded-sm" />
          <p className="truncate text-sm font-medium">{meta?.title || "Browser"}</p>
          {meta?.url && (
            <div className="col-start-2 min-w-0">
              <UrlBadge url={meta.url} />
            </div>
          )}
        </div>
        <Button variant="ghost" size="icon-sm" aria-label="Minimize browser" onClick={onMinimize}>
          <Minus />
        </Button>
      </div>

      <div className="flex min-h-0 flex-1 items-center justify-center overflow-hidden bg-muted/30 p-3">
        {frame ? (
          // eslint-disable-next-line @next/next/no-img-element
          <img
            ref={imgRef}
            src={`data:image/jpeg;base64,${frame.data}`}
            alt="Browser"
            tabIndex={0}
            draggable={false}
            className="max-h-full max-w-full cursor-default rounded-md shadow-sm outline-none ring-1 ring-border focus:ring-2 focus:ring-ring"
            onMouseMove={onMouseMove}
            onMouseDown={onMouseDown}
            onMouseUp={onMouseUp}
            onWheel={onWheel}
            onKeyDown={onKeyDown}
            onKeyUp={onKeyUp}
            onContextMenu={(e) => e.preventDefault()}
          />
        ) : status === "unavailable" ? (
          <p className="text-sm text-muted-foreground">The browser isn&apos;t running yet.</p>
        ) : status === "error" ? (
          <p className="text-sm text-destructive">Couldn&apos;t connect to the browser.</p>
        ) : (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            Connecting…
          </div>
        )}
      </div>
    </section>
  );
}

/** Minimized browser: a sticky thumbnail above the composer; click to reopen. */
export function BrowserThumbnail({
  frame,
  meta,
  onClick,
}: {
  frame: BrowserFrame | null;
  meta: BrowserMeta | null;
  onClick: () => void;
}) {
  return (
    // A div (not a button) so the hover URL input can nest and be copied without
    // triggering reopen; clicking the card otherwise reopens the full viewer.
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => (e.key === "Enter" || e.key === " ") && onClick()}
      className="flex w-full cursor-pointer items-center gap-3 rounded-xl border bg-muted/40 p-2 pr-3 text-left transition-colors hover:bg-muted"
    >
      <div className="relative h-11 w-16 shrink-0 overflow-hidden rounded-md bg-muted ring-1 ring-border">
        {frame ? (
          // eslint-disable-next-line @next/next/no-img-element
          <img
            src={`data:image/jpeg;base64,${frame.data}`}
            alt=""
            className="h-full w-full object-cover object-top"
          />
        ) : (
          <Globe className="absolute inset-0 m-auto size-4 text-muted-foreground" />
        )}
      </div>
      <div className="grid min-w-0 flex-1 grid-cols-[auto_1fr] items-center gap-x-1.5 leading-tight">
        <Favicon src={meta?.favicon} className="size-3.5 shrink-0 rounded-sm" />
        <p className="truncate text-sm font-medium">{meta?.title || "Browser"}</p>
        {meta?.url && (
          <div className="col-start-2 min-w-0">
            <UrlBadge url={meta.url} />
          </div>
        )}
      </div>
      <Maximize2 className="size-4 shrink-0 text-muted-foreground" />
    </div>
  );
}
