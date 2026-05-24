import { useEffect, useRef } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";
import "@xterm/xterm/css/xterm.css";

interface Props {
  sandboxId: string;
  serverUrl: string;
  sshHost: string;
  sshPort: number;
}

const FONT_FAMILY = '"MesloLGM Nerd Font Mono", Monaco, monospace';

export function Terminal({ sandboxId, serverUrl, sshHost, sshPort }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    let disposed = false;
    let cleanup = () => {};

    document.fonts.load(`13px ${FONT_FAMILY}`).finally(() => {
      if (disposed) return;

      const term = new XTerm({
        allowProposedApi: true,
        cursorBlink: false,
        cursorStyle: "block",
        fontSize: 13,
        lineHeight: 1,
        fontFamily: FONT_FAMILY,
        drawBoldTextInBrightColors: true,
        scrollback: 10000,
        theme: {
          background: "#000000",
          foreground: "#dddddd",
          cursor: "#dddddd",
          cursorAccent: "#000000",
          selectionBackground: "#4c83c4",
          selectionForeground: "#ffffff",
          black: "#000000",      brightBlack: "#686868",
          red: "#c91b00",        brightRed: "#ff6e67",
          green: "#00c200",      brightGreen: "#5ffa68",
          yellow: "#c7c400",     brightYellow: "#fffc67",
          blue: "#0225c7",       brightBlue: "#6871ff",
          magenta: "#c930c7",    brightMagenta: "#ff77ff",
          cyan: "#00c5c7",       brightCyan: "#60fdff",
          white: "#c7c7c7",      brightWhite: "#ffffff",
        },
      });

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
          const text = atob(b64);
          if (text) navigator.clipboard?.writeText(text).catch(() => {});
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
        const text = term.getSelection();
        if (text) navigator.clipboard?.writeText(text).catch(() => {});
      };
      el.addEventListener("mouseup", onMouseUp);

      // Cmd+C / Ctrl+Shift+C: use xterm's own key interception API so we run
      // inside xterm's trusted keydown handler. Return true so xterm also fires
      // the copy ClipboardEvent normally (double coverage).
      term.attachCustomKeyEventHandler((ev: KeyboardEvent) => {
        if (ev.type !== "keydown") return true;
        const isCopy = (ev.metaKey && ev.key === "c") || (ev.ctrlKey && ev.shiftKey && ev.key === "C");
        if (isCopy) {
          const text = term.getSelection();
          if (text) navigator.clipboard?.writeText(text).catch(() => {});
        }
        return true; // let xterm process the key normally (fires copy ClipboardEvent for Cmd+C)
      });

      let ws: WebSocket | null = null;
      let retryTimer: ReturnType<typeof setTimeout> | null = null;
      let everConnected = false;

      const ro = new ResizeObserver(() => {
        requestAnimationFrame(() => {
          fitAddon.fit();
          if (ws?.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
          }
        });
      });
      ro.observe(el);

      function connect() {
        if (disposed) return;

        const wsBase = serverUrl.replace(/^http/, "ws");
        const url = new URL(
          `/api/sandboxes/${encodeURIComponent(sandboxId)}/terminal`,
          wsBase,
        );
        url.searchParams.set("host", sshHost);
        url.searchParams.set("port", String(sshPort));
        url.searchParams.set("cols", String(term.cols));
        url.searchParams.set("rows", String(term.rows));

        ws = new WebSocket(url.toString());
        ws.binaryType = "arraybuffer";

        ws.onopen = () => {
          term.focus();
          ws!.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
        };
        ws.onmessage = (e) => {
          if (typeof e.data === "string") {
            try {
              const msg = JSON.parse(e.data);
              if (msg.type === "connected") { everConnected = true; return; }
            } catch { /* raw text, fall through */ }
          }
          term.write(e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : (e.data as string));
        };
        ws.onclose = () => {
          if (disposed) return;
          if (everConnected) {
            term.write("\r\n\x1b[2m[disconnected]\x1b[0m\r\n");
          } else {
            term.write("\r\n\x1b[33m[connecting…]\x1b[0m");
            retryTimer = setTimeout(connect, 2000);
          }
        };
        ws.onerror = () => {};
      }

      const rafId = requestAnimationFrame(() => {
        fitAddon.fit();
        connect();

        term.onData((data) => {
          if (ws?.readyState === WebSocket.OPEN) ws!.send(data);
        });
      });

      cleanup = () => {
        cancelAnimationFrame(rafId);
        ro.disconnect();
        el.removeEventListener("mouseup", onMouseUp);
        if (retryTimer !== null) clearTimeout(retryTimer);
        ws?.close();
        term.dispose();
      };
    });

    return () => {
      disposed = true;
      cleanup();
    };
  }, [sandboxId, serverUrl, sshHost, sshPort]);

  return <div ref={containerRef} className="h-full w-full overflow-hidden bg-[#000000] p-1" />;
}
