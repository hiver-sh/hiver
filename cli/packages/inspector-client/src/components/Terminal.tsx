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

      // We send the *intent* to resize (our panel's fit) to the server, but the
      // size we actually render at is decided by the program's output (below).
      // The server forces a repaint (SIGWINCH) on every resize it receives, so
      // re-sending an unchanged size turns ordinary output into a
      // resize→repaint→output feedback loop that pegs the CPU. Only emit on a
      // real change; reset to -1 on (re)connect so a fresh session gets it once.
      let lastSentCols = -1;
      let lastSentRows = -1;
      const sendResizeIntent = (cols: number, rows: number) => {
        if (!connected) return;
        if (cols === lastSentCols && rows === lastSentRows) return;
        lastSentCols = cols;
        lastSentRows = rows;
        sendInput({ type: "resize", cols, rows });
      };

      // The program tells us the grid it's actually drawing for: full-screen
      // apps position the cursor and set the scroll region in absolute terms.
      // Track the largest column/row addressed since the last screen epoch so we
      // render at the program's real size (and scroll a smaller panel) instead
      // of reflowing its output. This lets us adopt whatever size the server
      // settles on after our resize intent — including a shared terminal pinned
      // larger than our panel — with no server-side protocol change.
      const SIZE_CAP = 1000;
      let detCols = 0;
      let detRows = 0;
      let parseCarry = "";
      // Last panel fit (our resize intent), the floor the render grid falls back
      // to when no program is addressing the screen (e.g. a plain shell).
      let fitCols = 0;
      let fitRows = 0;
      const noteSize = (cols: number, rows: number) => {
        if (cols > detCols && cols <= SIZE_CAP) detCols = cols;
        if (rows > detRows && rows <= SIZE_CAP) detRows = rows;
      };

      // Reconcile the two sizes: the server's current emitted grid (detCols/
      // detRows, inferred from output) and our resize intent (fitCols/fitRows,
      // the panel fit). Render at the larger so the program's output is never
      // squeezed, and show a scrollbar only on the axis where the server's grid
      // overflows the panel — i.e. where the server did NOT resize down to our
      // intent. When it does resize to fit, both sizes agree and the scrollbars
      // disappear. Driven by both panel resizes and output, but never sends —
      // only the intent goes upstream, so this can't feed the repaint loop.
      const applyGrid = () => {
        const cols = Math.max(fitCols, detCols);
        const rows = Math.max(fitRows, detRows);
        if (cols < 1 || rows < 1) return;
        if (cols !== term.cols || rows !== term.rows) term.resize(cols, rows);
        el.style.overflowX = detCols > fitCols ? "auto" : "hidden";
        el.style.overflowY = detRows > fitRows ? "auto" : "hidden";
      };

      // Scan a raw output chunk for the absolute cursor/scroll-region escapes
      // (CSI H/f cursor pos, G column, d row, r scroll region, 8;rows;cols t),
      // carrying any half-received sequence over to the next chunk. A full-screen
      // clear (CSI 2J/3J) or an alternate-screen switch (DECSET/DECRST 1049/47/
      // 1047) starts a fresh repaint, so we reset the detected grid and let that
      // repaint rebuild it — that's how the server's emitted size (and the
      // scrollbars) follow the program when it resizes *down*, not just up.
      const scanForSize = (chunk: string) => {
        const s = parseCarry + chunk;
        let i = 0;
        let consumed = 0;
        while (i < s.length) {
          const esc = s.indexOf("\x1b[", i);
          if (esc === -1) {
            consumed = s.length;
            break;
          }
          let j = esc + 2;
          // CSI parameter bytes (0x30–0x3f) then intermediates (0x20–0x2f).
          while (
            j < s.length &&
            s.charCodeAt(j) >= 0x30 &&
            s.charCodeAt(j) <= 0x3f
          )
            j++;
          while (
            j < s.length &&
            s.charCodeAt(j) >= 0x20 &&
            s.charCodeAt(j) <= 0x2f
          )
            j++;
          if (j >= s.length) {
            consumed = esc; // incomplete CSI — re-parse from here next chunk
            break;
          }
          const final = s[j];
          const parts = s.slice(esc + 2, j).split(";");
          const num = (idx: number, def: number) => {
            const v = parseInt(parts[idx] ?? "", 10);
            return Number.isFinite(v) ? v : def;
          };
          // Cursor moves can be a size *probe*: apps jump to CSI 999;999H then
          // send CSI 6n (DSR) to read back the clamped position — the 999 is not
          // the real grid. Ignore a cursor move that is immediately followed by a
          // DSR. Defer one landing at the chunk edge so the 4-byte DSR look-ahead
          // can still see it once the next chunk arrives.
          const isCursorMove =
            final === "H" || final === "f" || final === "G" || final === "d";
          if (isCursorMove && s.length - (j + 1) < 4) {
            consumed = esc;
            break;
          }
          const isProbe = isCursorMove && s.startsWith("\x1b[6n", j + 1);
          if ((final === "H" || final === "f") && !isProbe)
            noteSize(num(1, 1), num(0, 1));
          else if (final === "G" && !isProbe) noteSize(num(0, 1), 0);
          else if (final === "d" && !isProbe) noteSize(0, num(0, 1));
          else if (final === "r") noteSize(0, num(1, 0));
          else if (final === "t" && num(0, 0) === 8)
            noteSize(num(2, 0), num(1, 0));
          else if (
            (final === "J" && (num(0, 0) === 2 || num(0, 0) === 3)) ||
            ((final === "h" || final === "l") &&
              (parts[0] === "?1049" ||
                parts[0] === "?47" ||
                parts[0] === "?1047"))
          ) {
            detCols = 0;
            detRows = 0;
          }
          i = j + 1;
          consumed = i;
        }
        parseCarry = s.slice(consumed);
        if (parseCarry.length > 64) parseCarry = parseCarry.slice(-64);
        applyGrid();
      };

      // Measure the panel, send that as our resize intent, and render to it
      // (clamped up to the program's grid). The program's response arrives as
      // output and is picked up by scanForSize.
      const refit = () => {
        const dims = fitAddon.proposeDimensions();
        if (!dims || !Number.isFinite(dims.cols) || !Number.isFinite(dims.rows))
          return;
        fitCols = dims.cols;
        fitRows = dims.rows;
        sendResizeIntent(dims.cols, dims.rows);
        applyGrid();
      };

      const ro = new ResizeObserver(() => requestAnimationFrame(refit));
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
          const bin = atob(base64);
          term.write(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
          scanForSize(bin);
        },
        onConnected: () => {
          connected = true;
          // A fresh server attach doesn't know our geometry yet; force a send.
          lastSentCols = -1;
          lastSentRows = -1;
          // Push our resize intent so the PTY repaints at the right geometry.
          if (fitCols > 0 && fitRows > 0) sendResizeIntent(fitCols, fitRows);
          term.focus();
        },
        onClose: () => {
          connected = false;
        },
      });

      const rafId = requestAnimationFrame(() => {
        refit();
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
