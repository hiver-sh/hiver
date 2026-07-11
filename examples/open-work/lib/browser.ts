import "server-only";

import WebSocket from "ws";

import { resolveBrowserCdpUrl } from "./hiver";

export type ScreencastFrame = {
  data: string; // base64 JPEG
  sessionId: number;
  metadata: { deviceWidth?: number; deviceHeight?: number };
};

/** The browser's current page, for the viewer chrome + minimized thumbnail. */
export type PageMeta = { title: string; url: string; favicon: string };

/** Normalized input event from the viewer (coords in the frame's natural px). */
export type BrowserInput =
  | { kind: "move"; x: number; y: number }
  | { kind: "down"; x: number; y: number; button: "left" | "right" | "middle" }
  | { kind: "up"; x: number; y: number; button: "left" | "right" | "middle" }
  | { kind: "wheel"; x: number; y: number; dx: number; dy: number }
  | {
      kind: "key";
      down: boolean;
      key: string;
      code: string;
      keyCode?: number;
      /** CDP modifier bitmask: Alt=1, Ctrl=2, Meta=4, Shift=8. */
      modifiers?: number;
      text?: string;
    }
  // Clipboard is bridged explicitly: the local clipboard never reaches the
  // remote page's own clipboard, so paste injects text and copy/cut reads the
  // remote selection back out.
  | { kind: "paste"; text: string }
  | { kind: "copy"; cut: boolean };

/** Result of an input dispatch; `text` carries the selection back for copy/cut. */
export type InputResult = { ok: boolean; text?: string };

/** Minimal CDP-over-WebSocket client that attaches to the page target. */
class CdpClient {
  private nextId = 1;
  private pending = new Map<number, { resolve: (v: unknown) => void; reject: (e: unknown) => void }>();
  private frameListeners = new Set<(f: ScreencastFrame) => void>();
  private metaListeners = new Set<(m: PageMeta) => void>();
  private pageSessionId = "";
  private pageTargetId = "";
  private closed = false;

  private constructor(private ws: WebSocket) {}

  static async connect(wsUrl: string): Promise<CdpClient> {
    const ws = new WebSocket(wsUrl);
    await new Promise<void>((resolve, reject) => {
      ws.once("open", () => resolve());
      ws.once("error", reject);
    });
    const client = new CdpClient(ws);
    ws.on("message", (d) => client.onMessage(d.toString()));
    ws.on("close", () => client.markClosed());
    ws.on("error", () => client.markClosed());
    await client.attachPage();
    return client;
  }

  get isClosed() {
    return this.closed;
  }

  private async attachPage() {
    const { targetInfos } = (await this.send("Target.getTargets")) as {
      targetInfos: { targetId: string; type: string }[];
    };
    let page = targetInfos.find((t) => t.type === "page");
    if (!page) {
      const created = (await this.send("Target.createTarget", {
        url: "about:blank",
      })) as { targetId: string };
      page = { targetId: created.targetId, type: "page" };
    }
    const { sessionId } = (await this.send("Target.attachToTarget", {
      targetId: page.targetId,
      flatten: true,
    })) as { sessionId: string };
    this.pageSessionId = sessionId;
    this.pageTargetId = page.targetId;
    await this.send("Page.enable", {}, sessionId);
  }

  /** Fetch the page's current title, URL, and favicon; notify meta listeners. */
  private async emitMeta() {
    if (this.metaListeners.size === 0) return;
    try {
      const { targetInfo } = (await this.send("Target.getTargetInfo", {
        targetId: this.pageTargetId,
      })) as { targetInfo: { title?: string; url?: string } };
      const url = targetInfo.url ?? "";
      const meta: PageMeta = {
        title: targetInfo.title ?? "",
        url,
        favicon: await this.faviconFor(url),
      };
      for (const l of this.metaListeners) l(meta);
    } catch {
      /* target gone */
    }
  }

  /** Resolve the page's favicon URL: the <link rel=icon>, else /favicon.ico. */
  private async faviconFor(url: string): Promise<string> {
    try {
      const { result } = (await this.send(
        "Runtime.evaluate",
        {
          expression:
            "(document.querySelector(\"link[rel~='icon']\")||{}).href || (location.origin + '/favicon.ico')",
          returnByValue: true,
        },
        this.pageSessionId,
      )) as { result: { value?: string } };
      if (result?.value) return result.value;
    } catch {
      /* not an HTML page (yet) */
    }
    try {
      return new URL(url).origin + "/favicon.ico";
    } catch {
      return "";
    }
  }

  send(method: string, params: unknown = {}, sessionId?: string): Promise<unknown> {
    const id = this.nextId++;
    const msg: Record<string, unknown> = { id, method, params };
    if (sessionId) msg.sessionId = sessionId;
    this.ws.send(JSON.stringify(msg));
    return new Promise((resolve, reject) => this.pending.set(id, { resolve, reject }));
  }

  private onMessage(raw: string) {
    let msg: Record<string, unknown>;
    try {
      msg = JSON.parse(raw);
    } catch {
      return;
    }
    if (typeof msg.id === "number" && this.pending.has(msg.id)) {
      const p = this.pending.get(msg.id)!;
      this.pending.delete(msg.id);
      if (msg.error) p.reject(new Error((msg.error as { message?: string }).message));
      else p.resolve(msg.result);
      return;
    }
    if (msg.method === "Page.screencastFrame") {
      const params = msg.params as ScreencastFrame;
      for (const l of this.frameListeners) l(params);
      // Ack so Chrome keeps sending frames.
      void this.send("Page.screencastFrameAck", { sessionId: params.sessionId }, this.pageSessionId).catch(() => {});
      return;
    }
    // Title/URL change as the page navigates → refresh the page metadata.
    if (msg.method === "Page.frameNavigated" || msg.method === "Page.loadEventFired") {
      void this.emitMeta();
    }
  }

  /** Subscribe to page title/URL updates; pushes the current value immediately. */
  onMeta(onMeta: (m: PageMeta) => void): () => void {
    this.metaListeners.add(onMeta);
    void this.emitMeta();
    return () => this.metaListeners.delete(onMeta);
  }

  async startScreencast(onFrame: (f: ScreencastFrame) => void): Promise<() => void> {
    const first = this.frameListeners.size === 0;
    this.frameListeners.add(onFrame);
    if (first) {
      await this.send(
        "Page.startScreencast",
        { format: "jpeg", quality: 60, everyNthFrame: 1, maxWidth: 1280, maxHeight: 800 },
        this.pageSessionId,
      );
    }
    return () => {
      this.frameListeners.delete(onFrame);
      if (this.frameListeners.size === 0) {
        void this.send("Page.stopScreencast", {}, this.pageSessionId).catch(() => {});
      }
    };
  }

  dispatch(method: string, params: unknown): Promise<unknown> {
    return this.send(method, params, this.pageSessionId);
  }

  /** Evaluate an expression in the page and return its value (for clipboard). */
  private async evaluate(expression: string): Promise<string> {
    try {
      const { result } = (await this.send(
        "Runtime.evaluate",
        { expression, returnByValue: true },
        this.pageSessionId,
      )) as { result: { value?: string } };
      return typeof result?.value === "string" ? result.value : "";
    } catch {
      return "";
    }
  }

  /** The current selection text — from a focused input/textarea or the page. */
  selectionText(): Promise<string> {
    return this.evaluate(`(() => {
      const el = document.activeElement;
      if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') && el.selectionStart != null) {
        return el.value.substring(el.selectionStart, el.selectionEnd);
      }
      return String(window.getSelection ? window.getSelection() : '');
    })()`);
  }

  /** Delete the current selection (for cut) in a form field or contenteditable. */
  deleteSelection(): Promise<string> {
    return this.evaluate(`(() => {
      const el = document.activeElement;
      if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') && el.selectionStart != null) {
        const s = el.selectionStart, e = el.selectionEnd;
        el.value = el.value.slice(0, s) + el.value.slice(e);
        el.selectionStart = el.selectionEnd = s;
        el.dispatchEvent(new Event('input', { bubbles: true }));
        return '';
      }
      if (document.execCommand) document.execCommand('delete');
      return '';
    })()`);
  }

  private markClosed() {
    this.closed = true;
    for (const p of this.pending.values()) p.reject(new Error("CDP connection closed"));
    this.pending.clear();
  }
}

// One CDP connection per task, shared by the screencast + input endpoints.
const clients = new Map<string, Promise<CdpClient>>();

async function getClient(taskKey: string): Promise<CdpClient | null> {
  // Reuse a live connection first (no Hiver round-trip when already connected).
  const existing = clients.get(taskKey);
  if (existing) {
    const c = await existing.catch(() => null);
    if (c && !c.isClosed) return c;
    clients.delete(taskKey);
  }

  // Otherwise ask Hiver where the browser VM is (reconciling our local map).
  const url = await resolveBrowserCdpUrl(taskKey);
  if (!url) return null;

  const p = CdpClient.connect(url);
  clients.set(taskKey, p);
  try {
    return await p;
  } catch (err) {
    clients.delete(taskKey);
    throw err;
  }
}

/**
 * Probe whether the task's browser VM is actually reachable: ask Hiver for the
 * sandbox (reconciling our local map), then dial its CDP WebSocket, giving up
 * after 1s. Fire-and-forget — the caller isn't blocked; `onAlive` runs only if
 * the socket opens (so a stale, torn-down VM never shows a dead viewer).
 */
export async function probeBrowser(taskKey: string, onAlive: () => void): Promise<void> {
  const url = await resolveBrowserCdpUrl(taskKey);
  if (!url) return;

  let settled = false;
  const ws = new WebSocket(url);
  const finish = (alive: boolean) => {
    if (settled) return;
    settled = true;
    clearTimeout(timer);
    try {
      ws.terminate();
    } catch {
      /* already closed */
    }
    if (alive) onAlive();
  };
  const timer = setTimeout(() => finish(false), 1000);
  ws.once("open", () => finish(true));
  ws.once("error", () => finish(false));
  ws.once("unexpected-response", () => finish(false));
}

/** Stream screencast frames + page metadata until `signal` aborts. */
export async function streamScreencast(
  taskKey: string,
  onFrame: (f: ScreencastFrame) => void,
  onMeta: (m: PageMeta) => void,
  signal: AbortSignal,
): Promise<boolean> {
  const client = await getClient(taskKey);
  if (!client) return false;
  const stopFrames = await client.startScreencast(onFrame);
  const stopMeta = client.onMeta(onMeta);
  signal.addEventListener(
    "abort",
    () => {
      stopFrames();
      stopMeta();
    },
    { once: true },
  );
  return true;
}

// Debounce reloads so approving several hosts at once triggers a single reload.
const reloadTimers = new Map<string, ReturnType<typeof setTimeout>>();

/**
 * Reload the current page in the task's browser VM — used after the user grants
 * egress, so the page that was blocked retries with the new access. Debounced and
 * fire-and-forget; a no-op when the task has no browser session.
 */
export function reloadBrowser(taskKey: string): void {
  const existing = reloadTimers.get(taskKey);
  if (existing) clearTimeout(existing);
  reloadTimers.set(
    taskKey,
    setTimeout(async () => {
      reloadTimers.delete(taskKey);
      try {
        const client = await getClient(taskKey);
        if (!client) return;
        await client.dispatch("Page.reload", {});
      } catch {
        /* no browser, or the reload failed */
      }
    }, 400),
  );
}

const CDP_BUTTON = { left: "left", right: "right", middle: "middle" } as const;

/** Translate a viewer input event into CDP Input commands. */
export async function dispatchInput(taskKey: string, e: BrowserInput): Promise<InputResult> {
  const client = await getClient(taskKey);
  if (!client) return { ok: false };

  switch (e.kind) {
    case "move":
      await client.dispatch("Input.dispatchMouseEvent", { type: "mouseMoved", x: e.x, y: e.y });
      break;
    case "down":
      await client.dispatch("Input.dispatchMouseEvent", {
        type: "mousePressed",
        x: e.x,
        y: e.y,
        button: CDP_BUTTON[e.button],
        clickCount: 1,
      });
      break;
    case "up":
      await client.dispatch("Input.dispatchMouseEvent", {
        type: "mouseReleased",
        x: e.x,
        y: e.y,
        button: CDP_BUTTON[e.button],
        clickCount: 1,
      });
      break;
    case "wheel":
      await client.dispatch("Input.dispatchMouseEvent", {
        type: "mouseWheel",
        x: e.x,
        y: e.y,
        deltaX: e.dx,
        deltaY: e.dy,
      });
      break;
    case "key":
      // Editing/navigation keys (Backspace, Enter, arrows, Tab…) carry no text;
      // Chrome only acts on them when given a virtual key code. Text keys use
      // "keyDown" (so the character is inserted); others use "rawKeyDown".
      await client.dispatch("Input.dispatchKeyEvent", {
        type: e.down ? (e.text ? "keyDown" : "rawKeyDown") : "keyUp",
        key: e.key,
        code: e.code,
        ...(e.modifiers ? { modifiers: e.modifiers } : {}),
        ...(e.keyCode
          ? { windowsVirtualKeyCode: e.keyCode, nativeVirtualKeyCode: e.keyCode }
          : {}),
        ...(e.text ? { text: e.text } : {}),
      });
      break;
    case "paste":
      // Inject the local clipboard text into the focused element.
      await client.dispatch("Input.insertText", { text: e.text });
      break;
    case "copy": {
      // Read the remote selection so the caller can put it on the local
      // clipboard; for cut, also delete it from the page.
      const text = await client.selectionText();
      if (e.cut) await client.deleteSelection();
      return { ok: true, text };
    }
  }
  return { ok: true };
}
