import { useEffect, useRef } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";
import "@xterm/xterm/css/xterm.css";

interface Props {
  sandboxId: string;
  serverUrl: string;
}

const FONT_FAMILY = '"MesloLGM Nerd Font Mono", Monaco, monospace';

export function Terminal({ sandboxId, serverUrl }: Props) {
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
      const webgl = new WebglAddon();
      webgl.onContextLoss(() => webgl.dispose());
      term.loadAddon(webgl);

      let ws: WebSocket | null = null;

      const ro = new ResizeObserver(() => {
        requestAnimationFrame(() => {
          fitAddon.fit();
          if (ws?.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
          }
        });
      });
      ro.observe(el);

      const rafId = requestAnimationFrame(() => {
        fitAddon.fit();

        const wsBase = serverUrl.replace(/^http/, "ws");
        const url = new URL(
          `/api/sandboxes/${encodeURIComponent(sandboxId)}/terminal`,
          wsBase,
        );
        url.searchParams.set("cols", String(term.cols));
        url.searchParams.set("rows", String(term.rows));

        ws = new WebSocket(url.toString());
        ws.binaryType = "arraybuffer";

        ws.onopen = () => {
          term.focus();
          ws!.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
        };
        ws.onmessage = (e) => {
          term.write(e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : (e.data as string));
        };
        ws.onclose = () => term.write("\r\n\x1b[2m[disconnected]\x1b[0m\r\n");
        ws.onerror = () => term.write("\r\n\x1b[31m[connection error]\x1b[0m\r\n");

        term.onData((data) => {
          if (ws?.readyState === WebSocket.OPEN) ws!.send(data);
        });
      });

      cleanup = () => {
        cancelAnimationFrame(rafId);
        ro.disconnect();

        ws?.close();
        term.dispose();
      };
    });

    return () => {
      disposed = true;
      cleanup();
    };
  }, [sandboxId, serverUrl]);

  return <div ref={containerRef} className="h-full w-full overflow-hidden bg-[#000000] p-1" />;
}
