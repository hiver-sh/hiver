import { useEffect, useRef } from "react";
import { useTransport } from "@/lib/transport";
import { useUserPreferences } from "@/lib/userPreferences";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";
import "@xterm/xterm/css/xterm.css";

/**
 * Output + lifecycle handlers the terminal registers on the shared per-sandbox
 * stream (owned by the parent). `onData` carries base64-encoded pty bytes.
 */
export interface TerminalSink {
  onData: (base64: string) => void;
  onConnected: () => void;
  onClose: () => void;
}

interface Props {
  sandboxKey: string;
  serverUrl: string;
  // Register on the parent's shared stream for terminal output/lifecycle.
  // Returns an unsubscribe function. The parent replays current state (a
  // `connected` + buffered scrollback) immediately if the terminal is already
  // attached, so subscribing late (e.g. opening the panel) still restores it.
  subscribe: (sink: TerminalSink) => () => void;
}

const FONT_FAMILY = "ui-monospace, Monaco, monospace";

const DARK_THEME = {
  background: "#000000",
  foreground: "#dddddd",
  cursor: "#dddddd",
  cursorAccent: "#000000",
  selectionBackground: "#4c83c4",
  selectionForeground: "#ffffff",
  black: "#000000",
  brightBlack: "#686868",
  red: "#c91b00",
  brightRed: "#ff6e67",
  green: "#00c200",
  brightGreen: "#5ffa68",
  yellow: "#c97800",
  brightYellow: "#ff8c00",
  blue: "#0225c7",
  brightBlue: "#6871ff",
  magenta: "#c930c7",
  brightMagenta: "#ff77ff",
  cyan: "#00c5c7",
  brightCyan: "#60fdff",
  white: "#c7c7c7",
  brightWhite: "#ffffff",
};

const LIGHT_THEME = {
  background: "#ffffff",
  foreground: "#000000",
  cursor: "#000000",
  cursorAccent: "#ffffff",
  selectionBackground: "#4c83c4",
  selectionForeground: "#ffffff",
  black: "#000000",
  brightBlack: "#444444",
  red: "#aa0000",
  brightRed: "#cc0000",
  green: "#005500",
  brightGreen: "#006600",
  yellow: "#c97800",
  brightYellow: "#ff8c00",
  blue: "#0000aa",
  brightBlue: "#0000cc",
  magenta: "#770077",
  brightMagenta: "#990099",
  cyan: "#006b6b",
  brightCyan: "#008080",
  white: "#888888",
  brightWhite: "#444444",
};

export function Terminal({ sandboxKey, serverUrl, subscribe }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const { transport } = useTransport();
  const { prefs, terminalScrollPassthrough } = useUserPreferences();

  // Read the latest preference from a ref so toggling it doesn't tear down and
  // recreate the terminal (which would drop the session).
  const clipboardCopyRef = useRef(prefs.terminalClipboardCopy);
  clipboardCopyRef.current = prefs.terminalClipboardCopy;
  const scrollPassthroughRef = useRef(terminalScrollPassthrough);
  scrollPassthroughRef.current = terminalScrollPassthrough;

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    let disposed = false;
    let cleanup = () => {};

    document.fonts.load(`13px ${FONT_FAMILY}`).finally(() => {
      if (disposed) return;

      const isDark = () => document.documentElement.classList.contains("dark");

      const term = new XTerm({
        allowProposedApi: true,
        cursorBlink: false,
        cursorStyle: "block",
        fontSize: 13,
        lineHeight: 1,
        fontFamily: FONT_FAMILY,
        drawBoldTextInBrightColors: true,
        scrollback: 10000,
        theme: isDark() ? DARK_THEME : LIGHT_THEME,
      });

      const themeObs = new MutationObserver(() => {
        term.options.theme = isDark() ? DARK_THEME : LIGHT_THEME;
        el.style.backgroundColor = isDark()
          ? DARK_THEME.background
          : LIGHT_THEME.background;
      });
      themeObs.observe(document.documentElement, {
        attributeFilter: ["class"],
      });

      // Skip clipboard writes when the user has disabled copy, which avoids the
      // browser's clipboard-permission prompt entirely.
      const copyToClipboard = (text: string) => {
        if (!text || !clipboardCopyRef.current) return;
        navigator.clipboard?.writeText(text).catch(() => {});
      };

      const fitAddon = new FitAddon();
      term.loadAddon(fitAddon);
      term.open(el);

      // OSC 52: tmux emits this when text is selected/copied (requires set-clipboard on in tmux.conf).
      // Format: \e]52;c;<base64-text>\a — decode and write to system clipboard.
      term.parser.registerOscHandler(52, (data) => {
        const idx = data.indexOf(";");
        if (idx === -1) return false;
        const b64 = data.slice(idx + 1);
        if (!b64 || b64 === "?") return false;
        try {
          copyToClipboard(atob(b64));
        } catch {
          /* invalid base64 */
        }
        return true;
      });

      const webgl = new WebglAddon();
      webgl.onContextLoss(() => webgl.dispose());
      term.loadAddon(webgl);

      // Copy-on-select: runs after xterm has finalized the selection on mouseup.
      // Only uses navigator.clipboard (no temp textarea) to avoid stealing focus
      // from xterm's textarea, which would clear the selection.
      const onMouseUp = () => {
        copyToClipboard(term.getSelection());
      };
      el.addEventListener("mouseup", onMouseUp);

      const onWheel = (ev: WheelEvent) => {
        if (scrollPassthroughRef.current) {
          ev.stopPropagation();
          // Prevent xterm from consuming the event, then re-dispatch on the
          // nearest scrollable ancestor so the page scrolls instead.
          const parent = el.parentElement;
          if (parent) parent.dispatchEvent(new WheelEvent("wheel", ev));
        }
      };
      el.addEventListener("wheel", onWheel, { capture: true });

      // Cmd+C / Ctrl+Shift+C: use xterm's own key interception API so we run
      // inside xterm's trusted keydown handler. Return true so xterm also fires
      // the copy ClipboardEvent normally (double coverage).
      term.attachCustomKeyEventHandler((ev: KeyboardEvent) => {
        if (ev.type !== "keydown") return true;
        const isCopy =
          (ev.metaKey && ev.key === "c") ||
          (ev.ctrlKey && ev.shiftKey && ev.key === "C");
        if (isCopy) {
          copyToClipboard(term.getSelection());
        }
        return true;
      });

      let connected = false;

      // Track the geometry we last told the server. The server forces a
      // repaint (SIGWINCH) on every resize it receives, so re-sending an
      // unchanged size turns ordinary output into a resize→repaint→output
      // feedback loop that pegs the CPU. Only emit on a real change; reset to
      // -1 on (re)connect so a fresh server session always gets the size once.
      let lastSentCols = -1;
      let lastSentRows = -1;
      const sendResizeIfChanged = () => {
        if (!connected) return;
        if (term.cols === lastSentCols && term.rows === lastSentRows) return;
        lastSentCols = term.cols;
        lastSentRows = term.rows;
        sendInput({ type: "resize", cols: term.cols, rows: term.rows });
      };

      const ro = new ResizeObserver(() => {
        requestAnimationFrame(() => {
          // Skip the fit when the cell grid wouldn't change: refitting to the
          // same size still churns layout and, with the server's
          // repaint-on-resize, can self-sustain a loop.
          const dims = fitAddon.proposeDimensions();
          if (
            !dims ||
            !Number.isFinite(dims.cols) ||
            !Number.isFinite(dims.rows) ||
            (dims.cols === term.cols && dims.rows === term.rows)
          ) {
            return;
          }
          fitAddon.fit();
          sendResizeIfChanged();
        });
      });
      ro.observe(el);

      function sendInput(msg: {
        type: string;
        data?: string;
        cols?: number;
        rows?: number;
      }) {
        if (!connected) return;
        const url = new URL(
          `/api/sandboxes/${encodeURIComponent(sandboxKey)}/terminal/input`,
          serverUrl,
        );
        transport
          .fetch(url.toString(), {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(msg),
          })
          .catch(() => {});
      }

      // Output + lifecycle come from the parent's shared per-sandbox stream
      // (one connection carries both the event feed and this terminal), instead
      // of the terminal holding its own SSE. Input/resize still go out as POSTs.
      const unsubscribe = subscribe({
        onData: (base64) => {
          term.write(Uint8Array.from(atob(base64), (c) => c.charCodeAt(0)));
        },
        onConnected: () => {
          connected = true;
          // A fresh server attach doesn't know our geometry yet; force a send.
          lastSentCols = -1;
          lastSentRows = -1;
          // Push the current size so the PTY repaints at the right geometry.
          sendResizeIfChanged();
          term.focus();
        },
        onClose: () => {
          connected = false;
        },
      });

      const rafId = requestAnimationFrame(() => {
        fitAddon.fit();
        term.onData((data) => sendInput({ type: "input", data }));
      });

      cleanup = () => {
        cancelAnimationFrame(rafId);
        ro.disconnect();
        themeObs.disconnect();
        el.removeEventListener("mouseup", onMouseUp);
        el.removeEventListener("wheel", onWheel, { capture: true });
        unsubscribe();
        term.dispose();
      };
    });

    return () => {
      disposed = true;
      cleanup();
    };
  }, [sandboxKey, serverUrl, transport, subscribe]);

  return (
    <div ref={containerRef} className="h-full w-full overflow-hidden p-1" />
  );
}
