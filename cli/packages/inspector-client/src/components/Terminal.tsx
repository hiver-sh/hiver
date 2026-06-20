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
  sandboxId: string;
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

export function Terminal({ sandboxId, sandboxKey, serverUrl, subscribe }: Props) {
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
      // Persistent UTF-8 decoder for the column scanner (see onData).
      const utf8 = new TextDecoder();

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

      // The sandbox PTY can't be resized (no SDK hook — the winsize is fixed), so
      // the program always draws at one width and a narrower panel would wrap it.
      // Instead we learn that width from the program's raw output and render xterm
      // at it, letting a smaller panel scroll horizontally. `detCols` is the
      // furthest column the program writes to since the last screen epoch; the
      // grid never goes below it, so output is never wrapped.
      const SIZE_CAP = 1000;
      let detCols = 0;
      // Virtual cursor column + carry for an escape split across chunks.
      let vcol = 0;
      let parseCarry = "";
      // Last panel fit: the grid floor when the program is narrower than the
      // panel (e.g. a plain shell), and our resize intent to the server.
      let fitCols = 0;
      let fitRows = 0;

      // Render the grid at least as wide as the program draws; scroll a narrower
      // panel rather than wrapping. Height tracks the panel (xterm scrolls its
      // scrollback vertically as usual), so only the width needs the override.
      const applyGrid = () => {
        const cols = Math.max(fitCols, detCols);
        if (cols < 1 || fitRows < 1) return;
        if (cols !== term.cols || fitRows !== term.rows)
          term.resize(cols, fitRows);
        el.style.overflowX = detCols > fitCols ? "auto" : "hidden";
      };

      // Run a minimal virtual terminal over the program's raw output to learn the
      // width it draws for: track the cursor column and record the furthest one
      // *written* to (bare cursor moves don't count, so size probes like
      // CSI 999;999H are ignored). A full-screen clear (CSI 2J/3J) or an
      // alternate-screen switch (DECSET/DECRST 1049/47/1047) starts a fresh epoch.
      // Reading the byte stream directly (not xterm's buffer) means this works on
      // the alternate screen too, which xterm never reflows.
      const noteCol = () => {
        if (vcol > detCols && vcol <= SIZE_CAP) detCols = vcol;
      };
      const scanForSize = (chunk: string) => {
        const s = parseCarry + chunk;
        const n = s.length;
        let i = 0;
        let consumed = 0;
        while (i < n) {
          const c = s.charCodeAt(i);
          if (c === 0x1b) {
            if (i + 1 >= n) break; // incomplete escape — carry from ESC
            const kind = s[i + 1];
            if (kind === "[") {
              let j = i + 2;
              while (
                j < n &&
                s.charCodeAt(j) >= 0x30 &&
                s.charCodeAt(j) <= 0x3f
              )
                j++;
              while (
                j < n &&
                s.charCodeAt(j) >= 0x20 &&
                s.charCodeAt(j) <= 0x2f
              )
                j++;
              if (j >= n) break; // incomplete CSI — carry
              const final = s[j];
              const p = s.slice(i + 2, j).split(";");
              const num = (idx: number, def: number) => {
                const v = parseInt(p[idx] ?? "", 10);
                return Number.isFinite(v) ? v : def;
              };
              if (final === "H" || final === "f")
                vcol = Math.max(0, num(1, 1) - 1);
              else if (final === "G" || final === "`")
                vcol = Math.max(0, num(0, 1) - 1);
              else if (final === "C" || final === "a") vcol += num(0, 1);
              else if (final === "D") vcol = Math.max(0, vcol - num(0, 1));
              else if (final === "E" || final === "F") vcol = 0;
              else if (final === "t" && num(0, 0) === 8) {
                if (num(2, 0) > detCols && num(2, 0) <= SIZE_CAP)
                  detCols = num(2, 0);
              } else if (
                (final === "J" && (num(0, 0) === 2 || num(0, 0) === 3)) ||
                ((final === "h" || final === "l") &&
                  (p[0] === "?1049" || p[0] === "?47" || p[0] === "?1047"))
              ) {
                detCols = 0;
                vcol = 0;
              }
              i = j + 1;
            } else if (kind === "]") {
              // OSC: skip to BEL or ST (ESC \) without counting the payload.
              let k = i + 2;
              while (k < n && s.charCodeAt(k) !== 0x07) {
                if (s.charCodeAt(k) === 0x1b && s[k + 1] === "\\") break;
                k++;
              }
              if (k >= n) break; // incomplete OSC — carry
              i = s.charCodeAt(k) === 0x1b ? k + 2 : k + 1;
            } else {
              i += 2; // two-char escape (ESC 7/8, ESC M, …)
            }
          } else if (c === 0x0d) {
            vcol = 0;
            i++;
          } else if (c === 0x08) {
            if (vcol > 0) vcol--;
            i++;
          } else if (c === 0x09) {
            vcol = (Math.floor(vcol / 8) + 1) * 8;
            i++;
          } else if (c < 0x20 || c === 0x7f) {
            i++; // LF and other controls: no column change
          } else {
            // Printable; an astral surrogate pair (usually wide) counts as 2.
            if (c >= 0xd800 && c <= 0xdbff) {
              vcol += 2;
              i += 2;
            } else {
              vcol += 1;
              i++;
            }
            noteCol();
          }
          consumed = i;
        }
        // Carry the trailing incomplete sequence whole (slicing it would corrupt
        // a long split escape, e.g. an OSC clipboard payload); abandon only a
        // pathologically unterminated one so the buffer stays bounded.
        parseCarry = s.slice(consumed);
        if (parseCarry.length > 65536) parseCarry = "";
        applyGrid();
      };

      // Measure the panel, send it as our resize intent, and use it as the grid
      // floor (the program's real width, from scanForSize, takes over when wider).
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
          `/api/sandboxes/${encodeURIComponent(sandboxId)}/${encodeURIComponent(sandboxKey)}/terminal/input`,
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
          const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
          term.write(bytes);
          // Decode as UTF-8 (streaming, so multibyte chars split across frames
          // are handled) so the column scanner counts characters, not bytes.
          scanForSize(utf8.decode(bytes, { stream: true }));
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
  }, [sandboxId, sandboxKey, serverUrl, transport, subscribe]);

  return (
    <div ref={containerRef} className="h-full w-full overflow-hidden p-1" />
  );
}
