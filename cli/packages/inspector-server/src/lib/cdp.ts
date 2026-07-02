import { Sandbox } from "@hiver.sh/client";
import WebSocket from "ws";
import { waitForSandbox } from "./waitForSandbox.js";

// The port chrome-headless-shell's CDP relay listens on inside the browser
// image (see docker/browser/Dockerfile `EXPOSE 9223`). Detection still probes
// every exposed port, but this lets us try the likely one first.
const LIKELY_CDP_PORT = 9223;

// The resident host fronts Chrome's DevTools endpoint with a stable `/cdp`
// alias on 0.0.0.0 (see docker/browser/chromehost). We reach it through the
// gateway proxy, upgraded to a WebSocket, exactly like skills/browser/cdp-bridge.js.
function cdpWsUrl(sandbox: Sandbox, port: number): string {
  return sandbox.proxyUrl(port).replace(/^http/, "ws") + "/cdp";
}


// Cache the detected CDP port per sandbox key so the stream handler and the
// status endpoint don't each re-probe. `undefined` = not yet probed;
// `null` = probed, no CDP port; a number = the detected port.
const detectedPort = new Map<string, number | null>();

// Open the `/cdp` relay and ask Chrome for its version. A real CDP endpoint
// answers Browser.getVersion with a product string ("HeadlessShell/..",
// rebranded to "Chrome/.." in our image); anything else isn't CDP. This mirrors
// how cdp-bridge.js connects (plain WS to the `/cdp` alias), which sidesteps
// Chrome's /json/* Host-header check that a gateway-proxied request would trip.
function probeCdp(sandbox: Sandbox, port: number): Promise<boolean> {
  return new Promise<boolean>((resolve) => {
    let settled = false;
    const done = (v: boolean) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        ws.close();
      } catch {
        /* ignore */
      }
      resolve(v);
    };
    const timer = setTimeout(() => done(false), 4000);
    let ws: WebSocket;
    try {
      ws = new WebSocket(cdpWsUrl(sandbox, port));
    } catch {
      clearTimeout(timer);
      resolve(false);
      return;
    }
    ws.addEventListener("open", () => {
      try {
        ws.send(JSON.stringify({ id: 1, method: "Browser.getVersion" }));
      } catch {
        done(false);
      }
    });
    ws.addEventListener("message", (ev) => {
      try {
        const msg = JSON.parse(String(ev.data)) as {
          id?: number;
          result?: { product?: string };
        };
        if (msg.id === 1) done(!!msg.result?.product);
      } catch {
        /* keep waiting for a valid frame until timeout */
      }
    });
    ws.addEventListener("error", () => done(false));
    ws.addEventListener("close", () => done(false));
  });
}

// Return the sandbox's CDP port, or null if none of its exposed ports speak
// CDP. Result is cached per key; pass `force` to re-probe.
export async function detectCdpPort(
  sandbox: Sandbox,
  { signal, force = false }: { signal?: AbortSignal; force?: boolean } = {},
): Promise<number | null> {
  const cached = detectedPort.get(sandbox.key);
  if (cached !== undefined && !force) return cached;

  let ports: number[];
  try {
    await waitForSandbox(sandbox, { signal });
    ports = await sandbox.getPorts();
  } catch {
    return null; // don't cache transient failures — allow a later retry
  }

  // Probe the conventional CDP port first, then the rest.
  const ordered = [
    ...ports.filter((p) => p === LIKELY_CDP_PORT),
    ...ports.filter((p) => p !== LIKELY_CDP_PORT),
  ];
  for (const port of ordered) {
    if (signal?.aborted) return null;
    if (await probeCdp(sandbox, port)) {
      detectedPort.set(sandbox.key, port);
      return port;
    }
  }
  detectedPort.set(sandbox.key, null);
  return null;
}


export interface BrowserClientHandle {
  // A JPEG screencast frame (base64) with its viewport dimensions in CSS px, so
  // the client can map click coordinates back to the page.
  sendFrame: (frame: { data: string; width: number; height: number }) => void;
  sendCtrl: (ev: string, d: object) => void;
  end: () => void;
}

// Input forwarded from the client, translated to CDP Input.* commands.
export type BrowserInput =
  | {
      type: "mouse";
      event: "move" | "down" | "up" | "wheel";
      x: number;
      y: number;
      button?: "none" | "left" | "middle" | "right";
      buttons?: number;
      clickCount?: number;
      deltaX?: number;
      deltaY?: number;
    }
  | { type: "key"; event: "down" | "up"; key: string; code: string; text?: string; keyCode?: number }
  | { type: "text"; text: string };

// Subset of CDP's Target.TargetInfo we read from discovery events.
interface TargetInfo {
  targetId: string;
  type: string;
  title?: string;
  url?: string;
}

interface BrowserSession {
  ws: WebSocket;
  ready: Promise<void>;
  // CDP flatten-mode session id for the attached page target.
  sessionId: string | null;
  // Latest viewport size reported by screencast metadata (CSS px).
  width: number;
  height: number;
  lastFrame: { data: string; width: number; height: number } | null;
  // URL of the attached page's main frame, tracked from Page.frameNavigated and
  // replayed to newly-connected clients so their address bar starts populated.
  currentUrl: string;
  // The page target the screencast is currently following (the active tab).
  currentTargetId: string | null;
  // All page targets, keyed by targetId — the tab strip. Kept current from
  // Target.targetCreated / targetDestroyed / targetInfoChanged discovery events.
  tabs: Map<string, { title: string; url: string }>;
  // Last CDP command error seen, surfaced in the client's error message so a
  // failed attach isn't just an opaque "no browser available".
  lastError: string;
  clients: Set<BrowserClientHandle>;
  nextId: number;
  pending: Map<number, (result: unknown) => void>;
  close: () => void;
}

const sessions = new Map<string, BrowserSession>();

function send(s: BrowserSession, method: string, params?: object): Promise<unknown> {
  const id = s.nextId++;
  const frame: Record<string, unknown> = { id, method };
  if (params) frame.params = params;
  if (s.sessionId) frame.sessionId = s.sessionId;
  return new Promise((resolve) => {
    s.pending.set(id, resolve);
    try {
      s.ws.send(JSON.stringify(frame));
    } catch {
      s.pending.delete(id);
      resolve(undefined);
    }
  });
}

// Send a command with an explicit (or no) session id, bypassing the session's
// current default — used before we've attached to a page target.
function sendOn(
  s: BrowserSession,
  method: string,
  params: object | undefined,
  sessionId: string | undefined,
): Promise<unknown> {
  const id = s.nextId++;
  const frame: Record<string, unknown> = { id, method };
  if (params) frame.params = params;
  if (sessionId) frame.sessionId = sessionId;
  return new Promise((resolve) => {
    s.pending.set(id, resolve);
    try {
      s.ws.send(JSON.stringify(frame));
    } catch {
      s.pending.delete(id);
      resolve(undefined);
    }
  });
}

// Attach the shared session to a page target (flatten mode) and start its
// screencast. Reused for the initial page and when switching to a new one.
async function attachAndScreencast(
  s: BrowserSession,
  targetId: string,
): Promise<void> {
  const attached = (await sendOn(
    s,
    "Target.attachToTarget",
    { targetId, flatten: true },
    undefined,
  )) as { sessionId?: string };
  if (!attached?.sessionId)
    throw new Error(s.lastError || "attach to page target failed");
  s.sessionId = attached.sessionId;
  s.currentTargetId = targetId;
  // Seed the address bar from what we already know about this target — a page
  // that finished loading before we attached fires no Page.frameNavigated, so
  // without this the bar would sit empty until the next navigation.
  s.currentUrl = s.tabs.get(targetId)?.url ?? "";
  await send(s, "Page.enable");
  await send(s, "Runtime.enable");
  await send(s, "Page.startScreencast", {
    format: "jpeg",
    quality: 60,
    maxWidth: 1280,
    maxHeight: 720,
    everyNthFrame: 1,
  });
}

// Point the shared screencast at a different page target: stop and detach the
// current one, then attach + screencast the new one. All watching clients
// follow, since they share this single session.
async function switchTarget(s: BrowserSession, targetId: string): Promise<void> {
  const old = s.sessionId;
  if (old) {
    await sendOn(s, "Page.stopScreencast", undefined, old).catch(() => {});
    await sendOn(s, "Target.detachFromTarget", { sessionId: old }, undefined).catch(
      () => {},
    );
  }
  s.sessionId = null;
  s.lastFrame = null;
  await attachAndScreencast(s, targetId);
  // Sync the address bar, tab highlight and back/forward state to the new page.
  s.currentUrl = s.tabs.get(targetId)?.url ?? "";
  for (const c of s.clients) c.sendCtrl("url", { url: s.currentUrl });
  broadcastTabs(s);
  await broadcastNavState(s);
}

// The tab strip as sent to clients: every page target, with the active one
// flagged so the client can highlight it.
function tabList(
  s: BrowserSession,
): { targetId: string; title: string; url: string; active: boolean }[] {
  return [...s.tabs].map(([targetId, t]) => ({
    targetId,
    title: t.title,
    url: t.url,
    active: targetId === s.currentTargetId,
  }));
}

function broadcastTabs(s: BrowserSession): void {
  const tabs = tabList(s);
  for (const c of s.clients) c.sendCtrl("tabs", { tabs });
}

// Whether the active page can go back / forward, derived from its navigation
// history. Chrome has no Page.goBack; you read the history and navigate to the
// neighbouring entry (see goHistory).
async function computeNavState(
  s: BrowserSession,
): Promise<{ canGoBack: boolean; canGoForward: boolean }> {
  const h = (await send(s, "Page.getNavigationHistory")) as {
    currentIndex?: number;
    entries?: unknown[];
  };
  const idx = h?.currentIndex ?? 0;
  const len = h?.entries?.length ?? 0;
  return { canGoBack: idx > 0, canGoForward: idx < len - 1 };
}

async function broadcastNavState(s: BrowserSession): Promise<void> {
  const ns = await computeNavState(s);
  for (const c of s.clients) c.sendCtrl("navstate", ns);
}

// Move the active page delta steps through its history (−1 back, +1 forward).
async function goHistory(s: BrowserSession, delta: number): Promise<void> {
  const h = (await send(s, "Page.getNavigationHistory")) as {
    currentIndex?: number;
    entries?: { id: number }[];
  };
  const idx = h?.currentIndex ?? 0;
  const entries = h?.entries ?? [];
  const target = idx + delta;
  if (target >= 0 && target < entries.length)
    await send(s, "Page.navigateToHistoryEntry", { entryId: entries[target].id });
}

function openSession(sandbox: Sandbox, port: number, key: string): BrowserSession {
  const ws = new WebSocket(cdpWsUrl(sandbox, port));
  const s: BrowserSession = {
    ws,
    ready: Promise.resolve(),
    sessionId: null,
    width: 0,
    height: 0,
    lastFrame: null,
    currentUrl: "",
    currentTargetId: null,
    tabs: new Map(),
    lastError: "",
    clients: new Set(),
    nextId: 1,
    pending: new Map(),
    close: () => {
      try {
        ws.close();
      } catch {
        /* ignore */
      }
    },
  };

  const teardown = () => {
    sessions.delete(key);
    for (const c of s.clients) {
      c.sendCtrl("close", {});
      c.end();
    }
    s.clients.clear();
  };

  s.ready = new Promise<void>((resolve, reject) => {
    ws.addEventListener("error", () => reject(new Error("cdp ws error")));
    ws.addEventListener("close", () => {
      teardown();
      reject(new Error("cdp ws closed"));
    });
    ws.addEventListener("open", () => {
      void (async () => {
        try {
          // Discover targets so we get targetCreated/Destroyed/InfoChanged events
          // for the tab strip, then seed the initial tab list.
          await sendOn(s, "Target.setDiscoverTargets", { discover: true }, undefined);
          const targets = (await sendOn(
            s,
            "Target.getTargets",
            undefined,
            undefined,
          )) as {
            targetInfos?: {
              targetId: string;
              type: string;
              title?: string;
              url?: string;
            }[];
          };
          for (const t of targets?.targetInfos ?? []) {
            if (t.type === "page")
              s.tabs.set(t.targetId, { title: t.title ?? "", url: t.url ?? "" });
          }
          let targetId = [...s.tabs.keys()][0];
          // A freshly-launched headless-shell may have no page target yet; make
          // one rather than failing the whole panel.
          if (!targetId) {
            const created = (await sendOn(
              s,
              "Target.createTarget",
              { url: "about:blank" },
              undefined,
            )) as { targetId?: string };
            targetId = created?.targetId ?? "";
            if (targetId) s.tabs.set(targetId, { title: "", url: "about:blank" });
          }
          if (!targetId) throw new Error(s.lastError || "no page target");
          await attachAndScreencast(s, targetId);
          resolve();
        } catch (err) {
          const reason = err instanceof Error ? err.message : String(err);
          console.error(`[cdp] ${key}: attach failed: ${reason}`);
          reject(new Error(reason));
          s.close();
        }
      })();
    });
  });

  ws.addEventListener("message", (ev) => {
    let msg: {
      id?: number;
      method?: string;
      params?: Record<string, unknown>;
      result?: unknown;
      error?: { message?: string; code?: number };
    };
    try {
      msg = JSON.parse(String(ev.data));
    } catch {
      return;
    }
    if (typeof msg.id === "number" && s.pending.has(msg.id)) {
      const cb = s.pending.get(msg.id)!;
      s.pending.delete(msg.id);
      if (msg.error) {
        s.lastError = msg.error.message ?? `CDP error ${msg.error.code}`;
        console.error(`[cdp] ${key}: command error: ${s.lastError}`);
      }
      cb(msg.result);
      return;
    }
    if (msg.method === "Page.screencastFrame") {
      const p = msg.params as {
        data: string;
        sessionId: number;
        metadata?: { deviceWidth?: number; deviceHeight?: number };
      };
      const width = Math.round(p.metadata?.deviceWidth ?? s.width);
      const height = Math.round(p.metadata?.deviceHeight ?? s.height);
      if (width) s.width = width;
      if (height) s.height = height;
      const frame = { data: p.data, width: s.width, height: s.height };
      s.lastFrame = frame;
      for (const c of s.clients) c.sendFrame(frame);
      // Ack so Chrome keeps sending frames.
      void send(s, "Page.screencastFrameAck", { sessionId: p.sessionId });
    } else if (msg.method === "Page.frameNavigated") {
      // Track main-frame navigations (no parentId) so the client's address bar
      // reflects where the page actually went — redirects, link clicks, etc.
      const f = (msg.params as { frame?: { url?: string; parentId?: string } })
        .frame;
      if (f && !f.parentId && f.url) {
        s.currentUrl = f.url;
        const tab = s.currentTargetId && s.tabs.get(s.currentTargetId);
        if (tab) tab.url = f.url;
        for (const c of s.clients) c.sendCtrl("url", { url: f.url });
        broadcastTabs(s);
        void broadcastNavState(s);
      }
    } else if (msg.method === "Target.targetCreated") {
      const t = (msg.params as { targetInfo?: TargetInfo }).targetInfo;
      if (t && t.type === "page") {
        s.tabs.set(t.targetId, { title: t.title ?? "", url: t.url ?? "" });
        broadcastTabs(s);
      }
    } else if (msg.method === "Target.targetInfoChanged") {
      const t = (msg.params as { targetInfo?: TargetInfo }).targetInfo;
      if (t && t.type === "page" && s.tabs.has(t.targetId)) {
        s.tabs.set(t.targetId, { title: t.title ?? "", url: t.url ?? "" });
        broadcastTabs(s);
        // Keep the address bar in sync when the *active* tab's URL changes,
        // including navigations that don't surface as Page.frameNavigated.
        if (t.targetId === s.currentTargetId && (t.url ?? "") !== s.currentUrl) {
          s.currentUrl = t.url ?? "";
          for (const c of s.clients) c.sendCtrl("url", { url: s.currentUrl });
        }
      }
    } else if (msg.method === "Target.targetDestroyed") {
      const targetId = (msg.params as { targetId?: string }).targetId;
      if (targetId && s.tabs.delete(targetId)) {
        broadcastTabs(s);
        // If the active tab was closed, follow another one (or open a blank
        // page if that was the last tab) so the screencast never goes dead.
        if (s.currentTargetId === targetId) {
          void (async () => {
            const next = [...s.tabs.keys()][0];
            if (next) {
              await switchTarget(s, next);
            } else {
              const created = (await sendOn(
                s,
                "Target.createTarget",
                { url: "about:blank" },
                undefined,
              )) as { targetId?: string };
              if (created?.targetId) {
                s.tabs.set(created.targetId, { title: "", url: "about:blank" });
                await switchTarget(s, created.targetId);
              }
            }
          })().catch(() => {});
        }
      }
    }
  });

  return s;
}

// attachBrowser wires a client into the sandbox's single shared CDP screencast
// session, opening the upstream on the first client and fanning frames out to
// the rest. Returns a detach function; the session is torn down once the last
// client leaves. Mirrors attachTerminal's shape so the stream handler treats
// both channels the same way.
export function attachBrowser(
  sandbox: Sandbox,
  port: number,
  handle: BrowserClientHandle,
): () => void {
  const key = sandbox.key;
  let s = sessions.get(key);
  if (!s) {
    s = openSession(sandbox, port, key);
    sessions.set(key, s);
    s.ready.catch(() => {
      /* handled via teardown/close paths */
    });
  }

  s.clients.add(handle);
  const session = s;
  let detached = false;

  session.ready
    .then(() => {
      if (detached) return;
      handle.sendCtrl("connected", { width: session.width, height: session.height });
      handle.sendCtrl("tabs", { tabs: tabList(session) });
      if (session.currentUrl) handle.sendCtrl("url", { url: session.currentUrl });
      if (session.lastFrame) handle.sendFrame(session.lastFrame);
      // Back/forward availability needs a history round-trip; send it to just
      // this client once it resolves.
      computeNavState(session)
        .then((ns) => {
          if (!detached) handle.sendCtrl("navstate", ns);
        })
        .catch(() => {});
    })
    .catch((err) => {
      if (detached) return;
      const message =
        err instanceof Error && err.message
          ? err.message
          : "no browser available";
      handle.sendCtrl("error", { message });
      handle.end();
    });

  return () => {
    detached = true;
    session.clients.delete(handle);
    if (session.clients.size === 0) {
      sessions.delete(key);
      session.close();
    }
  };
}

// Translate one client input event into the matching CDP Input.* command on the
// shared session. No-op if the browser session isn't attached yet.
export function browserInput(key: string, msg: BrowserInput): void {
  const s = sessions.get(key);
  if (!s || !s.sessionId) return;

  if (msg.type === "text") {
    void send(s, "Input.insertText", { text: msg.text });
    return;
  }
  if (msg.type === "key") {
    void send(s, "Input.dispatchKeyEvent", {
      type: msg.event === "down" ? "keyDown" : "keyUp",
      key: msg.key,
      code: msg.code,
      text: msg.text,
      windowsVirtualKeyCode: msg.keyCode,
      nativeVirtualKeyCode: msg.keyCode,
    });
    return;
  }
  // mouse
  const typeMap = {
    move: "mouseMoved",
    down: "mousePressed",
    up: "mouseReleased",
    wheel: "mouseWheel",
  } as const;
  void send(s, "Input.dispatchMouseEvent", {
    type: typeMap[msg.event],
    x: msg.x,
    y: msg.y,
    button: msg.button ?? "none",
    buttons: msg.buttons ?? 0,
    clickCount: msg.clickCount ?? 0,
    deltaX: msg.deltaX ?? 0,
    deltaY: msg.deltaY ?? 0,
  });
}

// Read the active page's current selection text (for copy/cut). The panel is a
// screencast image with no real selection of its own, so the "copy" has to come
// from inside the page — we evaluate window.getSelection() over CDP and hand the
// text back for the client to put on the local clipboard.
export async function browserGetSelection(key: string): Promise<string> {
  const s = sessions.get(key);
  if (!s || !s.sessionId) return "";
  const r = (await send(s, "Runtime.evaluate", {
    expression: "'' + (window.getSelection ? window.getSelection() : '')",
    returnByValue: true,
  })) as { result?: { value?: string } };
  return r?.result?.value ?? "";
}

// Navigation + tab control from the toolbar. Waits for the session to finish
// attaching so a control that arrives right after the panel opens isn't dropped.
export type BrowserControl =
  | { action: "navigate"; url: string }
  | { action: "newPage"; url?: string }
  | { action: "back" }
  | { action: "forward" }
  | { action: "activateTab"; targetId: string }
  | { action: "closeTab"; targetId: string };

export async function browserControl(
  key: string,
  cmd: BrowserControl,
): Promise<void> {
  const s = sessions.get(key);
  if (!s) return;
  await s.ready.catch(() => {});
  if (!s.sessionId) return;

  switch (cmd.action) {
    case "navigate":
      if (cmd.url) await send(s, "Page.navigate", { url: cmd.url });
      return;
    case "back":
      await goHistory(s, -1);
      return;
    case "forward":
      await goHistory(s, 1);
      return;
    case "activateTab":
      if (cmd.targetId !== s.currentTargetId && s.tabs.has(cmd.targetId))
        await switchTarget(s, cmd.targetId);
      return;
    case "closeTab":
      // Closing the active tab is handled by the Target.targetDestroyed event,
      // which switches the screencast to another tab (or a fresh one).
      await sendOn(s, "Target.closeTarget", { targetId: cmd.targetId }, undefined);
      return;
    case "newPage": {
      // Create a target at the requested URL (blank if none) and switch the
      // shared screencast to it. Target.createTarget is a browser-domain
      // command, so it goes out with no session id.
      const created = (await sendOn(
        s,
        "Target.createTarget",
        { url: cmd.url || "about:blank" },
        undefined,
      )) as { targetId?: string };
      if (created?.targetId) {
        if (!s.tabs.has(created.targetId))
          s.tabs.set(created.targetId, { title: "", url: cmd.url || "about:blank" });
        await switchTarget(s, created.targetId);
      }
      return;
    }
  }
}
