import { memo, useEffect, useRef, useState } from "react";
import { ArrowLeft, ArrowRight, RotateCw } from "lucide-react";
import { useTransport } from "@/lib/transport";

/** One open page target, as reflected in the tab strip. */
export interface BrowserTab {
  targetId: string;
  title: string;
  url: string;
  active: boolean;
}

/**
 * Frames + lifecycle the browser view registers on the shared per-sandbox
 * stream (owned by the parent). Frames carry a base64 JPEG plus the viewport
 * size in CSS px, so pointer coordinates can be mapped back to the page.
 */
export interface BrowserSink {
  onFrame: (frame: { data: string; width: number; height: number }) => void;
  // The browser may live in a nested sandbox, so connect carries which sandbox
  // (id/key) owns it — input/control/selection must be routed there, not to the
  // primary sandbox this panel is nested under.
  onConnected: (target: { id: string; key: string }) => void;
  onClose: () => void;
  // The page's current main-frame URL, pushed on connect and on every
  // navigation, so the address bar tracks where the page actually is.
  onUrl: (url: string) => void;
  // Whether the active page can go back/forward, for the toolbar buttons.
  onNavState: (state: { canGoBack: boolean; canGoForward: boolean }) => void;
  // The full set of open pages, for the tab strip.
  onTabs: (tabs: BrowserTab[]) => void;
}

// A short label for a tab: its title, else the URL host, else a placeholder.
export function tabLabel(tab: BrowserTab): string {
  if (tab.title) return tab.title;
  try {
    return new URL(tab.url).host || tab.url;
  } catch {
    return tab.url || "New tab";
  }
}

// Prefix a bare host/path with https:// so "example.com" navigates. Leaves URLs
// that already have a scheme (http, https, about:, data:, file:) untouched.
function normalizeUrl(raw: string): string {
  const url = raw.trim();
  if (!url) return "";
  if (/^[a-z][a-z0-9+.-]*:/i.test(url)) return url;
  return `https://${url}`;
}

interface Props {
  sandboxId: string;
  sandboxKey: string;
  serverUrl: string;
  // Register on the parent's shared stream for browser frames/lifecycle.
  // Returns an unsubscribe function.
  subscribe: (sink: BrowserSink) => () => void;
}

// DOM MouseEvent.button → CDP button name.
const BUTTONS = ["left", "middle", "right"] as const;

// Non-printable keys we forward as CDP key events, with the virtual-key code
// Chrome expects. Printable characters go through Input.insertText instead.
const KEY_CODES: Record<string, number> = {
  Enter: 13,
  Backspace: 8,
  Tab: 9,
  Escape: 27,
  ArrowUp: 38,
  ArrowDown: 40,
  ArrowLeft: 37,
  ArrowRight: 39,
  Delete: 46,
  Home: 36,
  End: 35,
  PageUp: 33,
  PageDown: 34,
};

function BrowserViewInner({
  sandboxId,
  sandboxKey,
  serverUrl,
  subscribe,
}: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const imgRef = useRef<HTMLImageElement>(null);
  const { transport, seekEpoch } = useTransport();

  // Latest viewport size (CSS px) reported with each frame, read by the input
  // handlers via a ref so they don't need to re-bind on every frame.
  const frameSizeRef = useRef({ width: 0, height: 0 });
  const connectedRef = useRef(false);
  // The sandbox that owns the attached browser (defaults to this panel's
  // primary; updated to the nested sandbox once we know which one has CDP). All
  // input/control/selection requests target this, so they hit the right one.
  const targetRef = useRef({ id: sandboxId, key: sandboxKey });

  // A backward scrub (which bumps seekEpoch) re-pumps the recorded stream from
  // the start. Drop the currently-painted frame so a stale future frame can't
  // linger if the seek lands before the next recorded frame — the re-pump then
  // repaints to the correct frame for the seeked position. No-op on mount and
  // in live sessions, where seekEpoch never changes after 0.
  useEffect(() => {
    imgRef.current?.removeAttribute("src");
    frameSizeRef.current = { width: 0, height: 0 };
  }, [seekEpoch]);

  // Address-bar value. While the user is editing it we stop mirroring the
  // page's live URL into it (tracked by urlFocusedRef) so their typing isn't
  // clobbered by an incoming navigation event.
  const [url, setUrl] = useState("");
  const urlFocusedRef = useRef(false);
  const [nav, setNav] = useState({ canGoBack: false, canGoForward: false });

  const postControl = (body: unknown) => {
    const { id, key } = targetRef.current;
    const u = new URL(
      `/api/sandboxes/${encodeURIComponent(id)}/${encodeURIComponent(key)}/browser/control`,
      serverUrl,
    );
    transport
      .fetch(u.toString(), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })
      .catch(() => {});
  };

  const navigate = (e: React.FormEvent) => {
    e.preventDefault();
    const target = normalizeUrl(url);
    if (!target) return;
    setUrl(target);
    postControl({ action: "navigate", url: target });
    containerRef.current?.focus();
  };

  useEffect(() => {
    const container = containerRef.current;
    const img = imgRef.current;
    if (!container || !img) return;

    // Route to the sandbox that owns the browser (may be nested), not the panel's
    // primary props.
    targetRef.current = { id: sandboxId, key: sandboxKey };

    const post = (msg: unknown) => {
      const { id, key } = targetRef.current;
      const url = new URL(
        `/api/sandboxes/${encodeURIComponent(id)}/${encodeURIComponent(key)}/browser/input`,
        serverUrl,
      );
      transport
        .fetch(url.toString(), {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(msg),
        })
        .catch(() => {});
    };

    // Copy: pull the remote page's selection and put it on the local clipboard.
    // Returns whether there was anything to copy (used by cut). Best-effort — a
    // blocked clipboard write (permissions/focus) just no-ops.
    const copySelection = async (): Promise<boolean> => {
      try {
        const { id, key } = targetRef.current;
        const u = new URL(
          `/api/sandboxes/${encodeURIComponent(id)}/${encodeURIComponent(key)}/browser/selection`,
          serverUrl,
        );
        const { text } = (await transport
          .fetch(u.toString())
          .then((r) => r.json())) as { text: string };
        if (!text) return false;
        await navigator.clipboard.writeText(text);
        return true;
      } catch {
        return false;
      }
    };

    // Map a pointer event on the (object-fit: contain) image to page CSS px.
    // The image is letterboxed inside the container, so compute the rendered
    // image rect from the frame aspect ratio, then scale into the page's size.
    // `clamp` keeps a drag/release that strays outside the image in-bounds so
    // the page still gets a mouseup (an out-of-bounds release would otherwise
    // leave the button stuck down, breaking the next click/selection).
    const toPageCoords = (
      e: MouseEvent,
      clamp = false,
    ): { x: number; y: number } | null => {
      const { width: fw, height: fh } = frameSizeRef.current;
      if (!fw || !fh) return null;
      const rect = container.getBoundingClientRect();
      const scale = Math.min(rect.width / fw, rect.height / fh);
      const renderedW = fw * scale;
      // The image is centered horizontally but top-aligned (items-start), so
      // only x is letterboxed; y starts at the top of the container.
      const offsetX = (rect.width - renderedW) / 2;
      const offsetY = 0;
      let px = (e.clientX - rect.left - offsetX) / scale;
      let py = (e.clientY - rect.top - offsetY) / scale;
      if (clamp) {
        px = Math.min(Math.max(px, 0), fw);
        py = Math.min(Math.max(py, 0), fh);
      } else if (px < 0 || py < 0 || px > fw || py > fh) {
        return null;
      }
      return { x: Math.round(px), y: Math.round(py) };
    };

    // The button currently held, so drag moves and the release carry it — this
    // is what lets a press-drag register as a text selection on the page rather
    // than a bare hover.
    let pressedButton: "none" | "left" | "middle" | "right" = "none";

    // Pointer moves fire far faster than we need to forward them; cap the rate
    // (~30/s) so hover/drag don't flood the input endpoint. Presses, releases,
    // wheels and keys are always sent immediately.
    let lastMove = 0;
    const throttled = () => {
      const now = performance.now();
      if (now - lastMove < 33) return false;
      lastMove = now;
      return true;
    };

    // Hover moves (no button down) are only meaningful over the image.
    const onHoverMove = (e: MouseEvent) => {
      if (pressedButton !== "none" || !throttled()) return;
      const p = toPageCoords(e);
      if (!p) return;
      post({ type: "mouse", event: "move", ...p, button: "none", buttons: e.buttons });
    };
    // Drag moves are tracked on the window (below) so a selection continues even
    // when the pointer leaves the panel; they carry the held button.
    const onDragMove = (e: MouseEvent) => {
      if (!throttled()) return;
      const p = toPageCoords(e, true);
      if (!p) return;
      post({ type: "mouse", event: "move", ...p, button: pressedButton, buttons: e.buttons });
    };
    const endDrag = (e: MouseEvent) => {
      const p = toPageCoords(e, true);
      const button =
        pressedButton !== "none" ? pressedButton : (BUTTONS[e.button] ?? "left");
      pressedButton = "none";
      window.removeEventListener("mousemove", onDragMove);
      window.removeEventListener("mouseup", endDrag);
      if (p)
        post({
          type: "mouse",
          event: "up",
          ...p,
          button,
          buttons: e.buttons,
          clickCount: e.detail || 1,
        });
    };
    const onMouseDown = (e: MouseEvent) => {
      const p = toPageCoords(e, true);
      if (!p) return;
      // Stop the browser starting its own (empty) text selection / image drag,
      // which would swallow the move/up events we need for a page selection.
      e.preventDefault();
      container.focus();
      pressedButton = BUTTONS[e.button] ?? "left";
      post({
        type: "mouse",
        event: "down",
        ...p,
        button: pressedButton,
        buttons: e.buttons,
        clickCount: e.detail || 1,
      });
      // Follow the drag on the window so it keeps tracking outside the panel and
      // always gets a release.
      window.addEventListener("mousemove", onDragMove);
      window.addEventListener("mouseup", endDrag);
    };
    const onWheel = (e: WheelEvent) => {
      const p = toPageCoords(e);
      if (!p) return;
      e.preventDefault();
      post({
        type: "mouse",
        event: "wheel",
        ...p,
        deltaX: e.deltaX,
        deltaY: e.deltaY,
      });
    };
    const onContextMenu = (e: MouseEvent) => e.preventDefault();

    const onKeyDown = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      // Clipboard shortcuts bridge the local clipboard to the remote page.
      if (mod && !e.altKey) {
        const k = e.key.toLowerCase();
        if (k === "c") {
          e.preventDefault();
          void copySelection();
          return;
        }
        if (k === "x") {
          e.preventDefault();
          // Cut = copy, then delete the selection (works in editable fields).
          void copySelection().then((had) => {
            if (had)
              post({
                type: "key",
                event: "down",
                key: "Delete",
                code: "Delete",
                keyCode: 46,
              });
          });
          return;
        }
        if (k === "v") {
          e.preventDefault();
          // Paste the local clipboard into the remote page as inserted text.
          navigator.clipboard
            .readText()
            .then((text) => {
              if (text) post({ type: "text", text });
            })
            .catch(() => {});
          return;
        }
      }
      // Printable character with no modifier → insert text directly.
      if (e.key.length === 1 && !e.ctrlKey && !e.metaKey) {
        e.preventDefault();
        post({ type: "text", text: e.key });
        return;
      }
      const code = KEY_CODES[e.key];
      if (code !== undefined) {
        e.preventDefault();
        post({
          type: "key",
          event: "down",
          key: e.key,
          code: e.code,
          keyCode: code,
        });
      }
    };
    const onKeyUp = (e: KeyboardEvent) => {
      const code = KEY_CODES[e.key];
      if (code !== undefined) {
        e.preventDefault();
        post({
          type: "key",
          event: "up",
          key: e.key,
          code: e.code,
          keyCode: code,
        });
      }
    };

    container.addEventListener("mousemove", onHoverMove);
    container.addEventListener("mousedown", onMouseDown);
    container.addEventListener("wheel", onWheel, { passive: false });
    container.addEventListener("contextmenu", onContextMenu);
    container.addEventListener("keydown", onKeyDown);
    container.addEventListener("keyup", onKeyUp);

    // Coalesce frames to the latest one per animation frame. During a seek
    // catch-up the recorded stream re-pumps hundreds of frames back-to-back,
    // but only the last is ever visible — decoding a base64 JPEG into `img.src`
    // for every intermediate frame was a top cost in the seek profile. Live
    // playback (frames slower than the display) is unaffected: each still paints.
    let pendingFrame: { data: string; width: number; height: number } | null =
      null;
    let frameRaf = 0;
    const applyFrame = () => {
      frameRaf = 0;
      if (!pendingFrame) return;
      frameSizeRef.current = {
        width: pendingFrame.width,
        height: pendingFrame.height,
      };
      img.src = `data:image/jpeg;base64,${pendingFrame.data}`;
      pendingFrame = null;
    };

    const unsubscribe = subscribe({
      onFrame: (frame) => {
        pendingFrame = frame;
        if (frameRaf === 0) frameRaf = requestAnimationFrame(applyFrame);
      },
      onConnected: (target) => {
        connectedRef.current = true;
        targetRef.current = target;
      },
      onClose: () => {
        connectedRef.current = false;
      },
      onUrl: (u) => {
        // Don't overwrite what the user is typing in the address bar. Show a
        // blank tab as an empty bar, like a real browser.
        if (!urlFocusedRef.current) setUrl(u === "about:blank" ? "" : u);
      },
      onNavState: setNav,
      // Tabs are rendered in the panel header (owned by the parent), so this
      // panel ignores the tab set.
      onTabs: () => {},
    });

    return () => {
      container.removeEventListener("mousemove", onHoverMove);
      container.removeEventListener("mousedown", onMouseDown);
      container.removeEventListener("wheel", onWheel);
      container.removeEventListener("contextmenu", onContextMenu);
      container.removeEventListener("keydown", onKeyDown);
      container.removeEventListener("keyup", onKeyUp);
      // Drop any in-flight drag listeners if we unmount mid-selection.
      window.removeEventListener("mousemove", onDragMove);
      window.removeEventListener("mouseup", endDrag);
      if (frameRaf !== 0) cancelAnimationFrame(frameRaf);
      unsubscribe();
    };
  }, [sandboxId, sandboxKey, serverUrl, transport, subscribe]);

  return (
    <div className="flex h-full w-full flex-col">
      {/* Browser chrome: the tab strip lives in the panel header (owned by the
          parent); this panel keeps the address bar. */}
      <div className="flex shrink-0 flex-col border-b border-border bg-background">
        {/* Address bar */}
        <form
          onSubmit={navigate}
          className="flex items-center gap-1 bg-muted/50 px-2 py-1.5"
        >
          <button
            type="button"
            onClick={() => postControl({ action: "back" })}
            disabled={!nav.canGoBack}
            title="Back"
            className="rounded p-1 text-foreground/70 transition-colors hover:bg-background hover:text-foreground disabled:pointer-events-none disabled:opacity-25"
          >
            <ArrowLeft className="h-4 w-4" />
          </button>
          <button
            type="button"
            onClick={() => postControl({ action: "forward" })}
            disabled={!nav.canGoForward}
            title="Forward"
            className="rounded p-1 text-foreground/70 transition-colors hover:bg-background hover:text-foreground disabled:pointer-events-none disabled:opacity-25"
          >
            <ArrowRight className="h-4 w-4" />
          </button>
          <button
            type="submit"
            title="Reload"
            className="rounded p-1 text-foreground/70 transition-colors hover:bg-background hover:text-foreground"
          >
            <RotateCw className="h-4 w-4" />
          </button>
          <input
            type="text"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            onFocus={(e) => {
              urlFocusedRef.current = true;
              e.target.select();
            }}
            onBlur={() => {
              urlFocusedRef.current = false;
            }}
            placeholder="Enter a URL…"
            spellCheck={false}
            className="min-w-0 flex-1 rounded-md border border-border bg-muted px-2.5 py-1 font-mono text-[11px] text-foreground outline-none transition-colors placeholder:text-muted-foreground/60 focus:border-blue-500/60"
          />
        </form>
      </div>
      <div
        ref={containerRef}
        tabIndex={0}
        className="flex min-h-0 w-full flex-1 items-start justify-center overflow-hidden bg-neutral-200 outline-none select-none dark:bg-black"
      >
        <img
          ref={imgRef}
          alt=""
          draggable={false}
          className="max-h-full max-w-full select-none object-contain"
        />
      </div>
    </div>
  );
}

// Memoized so unrelated SandboxDetail re-renders don't re-render the live
// browser canvas; it re-renders only when its own props change.
export const BrowserView = memo(BrowserViewInner);
