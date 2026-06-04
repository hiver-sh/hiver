import { useEffect, useRef } from "react";
import { useTransport } from "@/lib/transport";
import { useUserPreferences } from "@/lib/userPreferences";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";
import "@xterm/xterm/css/xterm.css";

interface Props {
  sandboxId: string;
  serverUrl: string;
  exposedEndpoint?: string;
  initCommand?: string;
}

const FONT_FAMILY = '"MesloLGM Nerd Font Mono", Monaco, monospace';

const DARK_THEME = {
  background: "#000000",
  foreground: "#dddddd",
  cursor: "#dddddd",
  cursorAccent: "#000000",
  selectionBackground: "#4c83c4",
  selectionForeground: "#ffffff",
  black: "#000000",      brightBlack: "#686868",
  red: "#c91b00",        brightRed: "#ff6e67",
  green: "#00c200",      brightGreen: "#5ffa68",
  yellow: "#c97800",     brightYellow: "#ff8c00",
  blue: "#0225c7",       brightBlue: "#6871ff",
  magenta: "#c930c7",    brightMagenta: "#ff77ff",
  cyan: "#00c5c7",       brightCyan: "#60fdff",
  white: "#c7c7c7",      brightWhite: "#ffffff",
};

const LIGHT_THEME = {
  background: "#ffffff",
  foreground: "#000000",
  cursor: "#000000",
  cursorAccent: "#ffffff",
  selectionBackground: "#4c83c4",
  selectionForeground: "#ffffff",
  black: "#000000",      brightBlack: "#444444",
  red: "#aa0000",        brightRed: "#cc0000",
  green: "#005500",      brightGreen: "#006600",
  yellow: "#c97800",     brightYellow: "#ff8c00",
  blue: "#0000aa",       brightBlue: "#0000cc",
  magenta: "#770077",    brightMagenta: "#990099",
  cyan: "#006b6b",       brightCyan: "#008080",
  white: "#888888",      brightWhite: "#444444",
};

export function Terminal({ sandboxId, serverUrl, exposedEndpoint, initCommand }: Props) {
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
        el.style.backgroundColor = isDark() ? DARK_THEME.background : LIGHT_THEME.background;
      });
      themeObs.observe(document.documentElement, { attributeFilter: ["class"] });

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
        } catch { /* invalid base64 */ }
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
        const isCopy = (ev.metaKey && ev.key === "c") || (ev.ctrlKey && ev.shiftKey && ev.key === "C");
        if (isCopy) {
          copyToClipboard(term.getSelection());
        }
        return true;
      });

      const sessionId = crypto.randomUUID();
      let abortCtrl: AbortController | null = null;
      let retryTimer: ReturnType<typeof setTimeout> | null = null;
      let everConnected = false;
      let connected = false;

      const ro = new ResizeObserver(() => {
        requestAnimationFrame(() => {
          fitAddon.fit();
          if (connected) sendInput({ type: "resize", cols: term.cols, rows: term.rows });
        });
      });
      ro.observe(el);

      function sendInput(msg: { type: string; data?: string; cols?: number; rows?: number }) {
        if (!connected) return;
        const url = new URL(
          `/api/sandboxes/${encodeURIComponent(sandboxId)}/terminal/input`,
          serverUrl,
        );
        url.searchParams.set("sessionId", sessionId);
        transport.fetch(url.toString(), {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(msg),
        }).catch(() => {});
      }

      async function connect() {
        if (disposed) return;

        connected = false;
        abortCtrl = new AbortController();

        const url = new URL(
          `/api/sandboxes/${encodeURIComponent(sandboxId)}/terminal/stream`,
          serverUrl,
        );
        url.searchParams.set("cols", String(term.cols));
        url.searchParams.set("rows", String(term.rows));
        url.searchParams.set("sessionId", sessionId);
        if (exposedEndpoint) url.searchParams.set("exposedBackend", exposedEndpoint);
        if (initCommand) url.searchParams.set("initCommand", initCommand);

        let resp: Response;
        try {
          resp = await transport.fetch(url.toString(), { signal: abortCtrl.signal });
        } catch {
          if (!disposed) retryTimer = setTimeout(connect, 2000);
          return;
        }

        if (!resp.ok || !resp.body) {
          if (!disposed) retryTimer = setTimeout(connect, 2000);
          return;
        }

        const reader = resp.body.getReader();
        const dec = new TextDecoder();
        let buf = "";

        outer: while (true) {
          let done: boolean, value: Uint8Array | undefined;
          try {
            ({ done, value } = await reader.read());
          } catch {
            break;
          }
          if (done) break;
          buf += dec.decode(value, { stream: true });

          let sep: number;
          while ((sep = buf.indexOf("\n\n")) !== -1) {
            const block = buf.slice(0, sep);
            buf = buf.slice(sep + 2);

            let eventName = "message";
            let dataLine = "";
            for (const line of block.split("\n")) {
              if (line.startsWith("event: ")) eventName = line.slice(7);
              else if (line.startsWith("data: ")) dataLine = line.slice(6);
            }

            if (eventName === "connected") {
              everConnected = true;
              connected = true;
              // Push the current size immediately: the PTY starts at a default
              // geometry and the ResizeObserver's initial fire was dropped while
              // still unconnected, so without this the remote process never
              // learns the real dimensions and its repaints land at the wrong
              // size (stale content survives screen transitions).
              sendInput({ type: "resize", cols: term.cols, rows: term.rows });
              term.focus();
            } else if (eventName === "close") {
              connected = false;
              break outer;
            } else if (eventName === "message" && dataLine) {
              term.write(Uint8Array.from(atob(dataLine), (c) => c.charCodeAt(0)));
            }
          }
        }

        if (!disposed && !everConnected) {
          retryTimer = setTimeout(connect, 2000);
        }
      }

      const rafId = requestAnimationFrame(() => {
        fitAddon.fit();
        connect();
        term.onData((data) => sendInput({ type: "input", data }));
      });

      cleanup = () => {
        cancelAnimationFrame(rafId);
        ro.disconnect();
        themeObs.disconnect();
        el.removeEventListener("mouseup", onMouseUp);
        el.removeEventListener("wheel", onWheel, { capture: true });
        if (retryTimer !== null) clearTimeout(retryTimer);
        abortCtrl?.abort();
        term.dispose();
      };
    });

    return () => {
      disposed = true;
      cleanup();
    };
  }, [sandboxId, serverUrl, exposedEndpoint, transport]);

  return <div ref={containerRef} className="h-full w-full overflow-hidden p-1" />;
}
