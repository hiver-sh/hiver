// Resident Chrome, driven over CDP.
//
// This is the image entrypoint, so it runs during the prewarm boot and stays
// resident — every sandbox claimed from the warm pool inherits Chromium already
// launched and listening (captured in the microvm snapshot, or kept alive in the
// runc container).
//
// We spawn the Playwright-managed Chromium binary directly with its DevTools (CDP)
// endpoint open and supervise it. The only Playwright that runs here is a one-shot
// pre-snapshot warmup (see warmup()), which attaches over CDP and opens a page so a
// resumed sandbox starts warm; it does not drive the browser for clients. An
// external Playwright host attaches to a STABLE url with
//
//     const browser = await chromium.connectOverCDP("ws://<host>:9223/cdp")
//
// reached through the sandbox ingress proxy (/v1/<key>/proxy/9223/cdp).
// connectOverCDP speaks raw CDP over WebSocket, so the attaching host can run any
// Playwright version — it need not match the Chromium baked into this image.
//
// Why a relay (not a plain forwarder): Chrome's real browser endpoint is
// /devtools/browser/<uuid>, with a fresh <uuid> each launch, so a normal client
// has to GET /json/version and re-derive that path every time. We front Chrome
// with a small relay that exposes the fixed /cdp alias and rewrites it onto the
// current GUID (resolved once at startup) — the client gets one durable URL.
//
// Two more reasons the relay (vs. Chrome binding the port itself): Chrome keeps
// its DevTools socket on loopback regardless of --remote-debugging-address=0.0.0.0
// (a known headless quirk), so a cross-netns dial to the guest IP
// gets "connection refused"; the relay binds 0.0.0.0 like the old browser-host
// did. And before the snapshot we resolve the GUID and pre-open an about:blank
// page, so a resumed VM is warm (a usable page, no renderer spawn on first use).
//
// Readiness signal: Chrome prints "DevTools listening on ws://..." to stderr once
// the endpoint is up; we write READY_FILE then. Under microvm isolation sbxguest
// waits for that file before letting the host snapshot the (now warm) VM. Under
// runc isolation the file is unused — container readiness is the poststart fifo.
const { spawn } = require("child_process");
const { chromium } = require("playwright-core");
const fs = require("fs");
const http = require("http");
const net = require("net");
const os = require("os");
const path = require("path");

// External TCP port the sandbox ingress proxy dials (/v1/<key>/proxy/<port>),
// reachable at the guest's IP. Keep in sync with the client default.
//
// Chrome binds 127.0.0.1:CHROME_CDP_PORT (loopback only, see header); the relay
// below listens on 0.0.0.0:PORT and is what the ingress proxy actually dials.
const PORT = Number(process.env.HIVER_BROWSER_PORT || "9223");
// Loopback port Chrome's DevTools/CDP endpoint actually binds.
const CHROME_CDP_PORT = Number(process.env.HIVER_CHROME_CDP_PORT || "9222");
const READY_FILE = process.env.HIVER_PREWARM_READY_FILE || "/run/hiver/prewarm-ready";
// Fresh, empty profile per launch in a throwaway dir. chrome-headless-shell has
// no sign-in/GCM subsystems to write state into it, so it stays clean regardless.
const USER_DATA_DIR = fs.mkdtempSync(path.join(os.tmpdir(), "hiver-chrome-"));
// Stable symlink to the chrome-headless-shell binary, created at image build time
// (see the Dockerfile) — so nothing at runtime depends on Playwright.
const CHROME_BIN = "/opt/hiver/chrome";

// Chromium launch flags. Two concerns only:
//   1. The CDP endpoint, so the relay/client can attach.
//   2. Cost: the resident host keeps one browser alive per sandbox, so its idle
//      footprint and per-navigation CPU set how many browsers a node can pack. We
//      collapse the process model to a single process (no renderer/zygote forks),
//      drop the GPU/SwiftShader path (no paint needed for DOMContentLoaded
//      automation), disable image decode, and shrink every internal cache.
//
// There are deliberately NO anti-telemetry flags here. The binary is
// chrome-headless-shell (the standalone successor to old --headless; see the
// Dockerfile), which has no profile/sign-in (GAIA), GCM, search-engine, or
// background-networking subsystems compiled in — it makes zero network requests
// until told to navigate. The long --disable-features / --disable-background-*
// / --host-resolver-rules list the full browser (--headless=new) needed to stay
// quiet is therefore unnecessary.
//
// NOTE: --single-process is not officially supported by Chromium and can crash on
// some heavy/complex pages. We accept that here because this host drives simple
// navigation/automation workloads where the memory savings (one process instead
// of browser+zygote+gpu+renderer) dominate. If a workload starts crashing,
// dropping --single-process/--no-zygote is the first thing to revert.
const args = [
  // --- DevTools/CDP endpoint ---
  // Chrome binds this on loopback only (see PORT/CHROME_CDP_PORT note above); the
  // 0.0.0.0 relay below is what the ingress proxy actually reaches.
  // --remote-allow-origins=* is required by Chrome 111+: without it Chrome
  // rejects the WebSocket upgrade with HTTP 403 on its origin check, which is
  // exactly the connectOverCDP handshake.
  `--remote-debugging-port=${CHROME_CDP_PORT}`,
  "--remote-allow-origins=*",
  // a fresh empty profile (see USER_DATA_DIR note above). No --headless flag:
  // chrome-headless-shell is headless by definition.
  `--user-data-dir=${USER_DATA_DIR}`,

  // --- cost-minimizing flags ---
  // runs as the guest's root init, so the sandbox is required.
  "--no-sandbox",
  // one process for everything: no zygote pre-fork, no separate renderer/GPU
  // processes — the lowest possible resident memory.
  "--single-process",
  "--no-zygote",
  // collapse the process model further (belt-and-suspenders if single-process is
  // ever reverted): one renderer reused across (cross-origin) navigations.
  "--disable-features=site-per-process,IsolateOrigins",
  "--disable-site-isolation-trials",
  "--process-per-site",
  "--renderer-process-limit=1",
  "--in-process-gpu",
  // no paint needed for DOMContentLoaded — drop GPU + software rasterizer.
  "--disable-gpu",
  "--disable-software-rasterizer",
  "--disable-accelerated-2d-canvas",
  // skip image decode (cuts CPU/memory; irrelevant to DOMContentLoaded timing).
  "--blink-settings=imagesEnabled=false",
  // shrink caches/heap and surface area → lower resident RSS.
  "--enable-low-end-device-mode",
  "--disk-cache-size=1",
  "--media-cache-size=1",
  "--js-flags=--max-old-space-size=128",
  "--window-size=800,600",
  "--mute-audio",
  // a 1 GiB guest's default /dev/shm is tiny; use /tmp instead.
  "--disable-dev-shm-usage",
];

// Spawn the chrome-headless-shell binary that `npx playwright install` placed
// under PLAYWRIGHT_BROWSERS_PATH at build time. Its path was baked into CHROME_BIN;
// nothing here touches Playwright at runtime.
const child = spawn(CHROME_BIN, args, {
  stdio: ["ignore", "inherit", "pipe"],
});

// Stable CDP URL the client connects to (…/proxy/9223/cdp). Chrome's real
// browser endpoint is /devtools/browser/<uuid>, where <uuid> is freshly minted
// on every launch — so without this the client must fetch /json/version and
// re-derive the path each time. The relay below maps this fixed alias onto
// whatever GUID Chrome currently has, resolved once at startup (browserWsPath).
const STABLE_PATH = "/cdp";
let browserWsPath = null;

// Resolve Chrome's current browser WebSocket path (/devtools/browser/<uuid>) by
// reading its /json/version. Hitting this also warms Chrome's CDP HTTP surface
// before the snapshot.
function resolveBrowserWsPath() {
  return new Promise((resolve, reject) => {
    const req = http.get(
      { host: "127.0.0.1", port: CHROME_CDP_PORT, path: "/json/version", timeout: 5000 },
      (res) => {
        let b = "";
        res.on("data", (d) => (b += d));
        res.on("end", () => {
          try {
            resolve(new URL(JSON.parse(b).webSocketDebuggerUrl).pathname);
          } catch (e) {
            reject(e);
          }
        });
      },
    );
    req.on("error", reject);
    req.on("timeout", () => {
      req.destroy();
      reject(new Error("timeout"));
    });
  });
}

// CDP_WARMUP_TIMEOUT_MS bounds the pre-snapshot Playwright warmup. It's
// best-effort, so on timeout we proceed to signal readiness regardless.
const CDP_WARMUP_TIMEOUT_MS = Number(process.env.HIVER_CDP_WARMUP_TIMEOUT_MS || "10000");

// warmup drives one real CDP attach with Playwright before the snapshot is taken,
// so a resumed sandbox starts warm instead of paying cold-start on the first client
// attach. It connects over the *relay's* stable ws://…/cdp endpoint — the exact
// path real clients use — so the relay's first-connection handling is warmed too,
// alongside Chrome accepting its first WebSocket upgrade and initializing its
// DevTools surface. Opening a blank page spawns that page's renderer; all of this is
// captured in the snapshot, so the next client's connectOverCDP and first navigation
// skip the renderer spawn + first-attach work that dominated the measured cold
// connect. This is the only place Playwright runs in the image; it replaces the old
// raw /json/new pre-open (Playwright now owns the page lifecycle). browser.close()
// only disconnects this client — Chrome and the opened page stay resident (verified:
// the page survives into the live browser). Best-effort and time-bounded so a warmup
// failure never blocks readiness. Requires the relay listening and browserWsPath
// resolved (the relay rewrites /cdp onto it).
async function warmup() {
  let browser;
  try {
    browser = await chromium.connectOverCDP(`ws://127.0.0.1:${PORT}${STABLE_PATH}`, {
      timeout: CDP_WARMUP_TIMEOUT_MS,
    });
    const context = browser.contexts()[0] ?? (await browser.newContext());
    const page = await context.newPage();
    await page.goto("about:blank");
  } catch (e) {
    console.error("hiver chrome CDP host: cdp warmup failed:", e.message);
  } finally {
    try {
      await browser?.close();
    } catch {}
  }
}

// 0.0.0.0 relay in front of Chrome's loopback CDP endpoint. It's HTTP-aware only
// to the extent of rewriting the request line: a connection to STABLE_PATH has
// its path swapped for Chrome's real browser GUID path; everything else
// (/json/version, /json/list, an already-resolved /devtools/... path) passes
// through untouched. After the request line it's a raw byte pipe, so the CDP
// WebSocket frames flow through unmodified.
const relay = net.createServer((client) => {
  let buf = Buffer.alloc(0);
  const onData = (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    const nl = buf.indexOf("\r\n");
    if (nl === -1) {
      if (buf.length > 8192) client.destroy(); // request line should arrive promptly
      return;
    }
    client.off("data", onData);

    // "GET /cdp HTTP/1.1" → swap the path for Chrome's browser GUID path.
    let head = buf;
    const reqLine = buf.slice(0, nl).toString("latin1");
    const m = reqLine.match(/^(\S+)\s+(\S+)\s+(HTTP\/\d\.\d)$/);
    if (m && browserWsPath && (m[2] === STABLE_PATH || m[2] === STABLE_PATH + "/")) {
      const rewritten = `${m[1]} ${browserWsPath} ${m[3]}`;
      head = Buffer.concat([Buffer.from(rewritten, "latin1"), buf.slice(nl)]);
    }

    const upstream = net.connect(CHROME_CDP_PORT, "127.0.0.1", () => {
      upstream.write(head);
      client.pipe(upstream);
      upstream.pipe(client);
    });
    const close = () => {
      client.destroy();
      upstream.destroy();
    };
    client.on("error", close);
    upstream.on("error", close);
  };
  client.on("data", onData);
  client.on("error", () => client.destroy());
});
relay.on("error", (e) => {
  console.error("hiver chrome CDP host: relay error:", e);
  process.exit(1);
});

let started = false;
child.stderr.on("data", (buf) => {
  const s = buf.toString();
  process.stderr.write(s);
  // Chrome prints e.g. "DevTools listening on ws://127.0.0.1:9222/devtools/browser/<id>"
  // once the CDP endpoint is accepting connections. Only then resolve the stable
  // path, bring up the relay, warm the CDP attach path through it with Playwright,
  // and signal readiness — so we never snapshot before Chrome (and a warm page) can
  // serve, and the warmup exercises the same relay path real clients use.
  if (!started && /DevTools listening on ws:\/\//.test(s)) {
    started = true;
    (async () => {
      try {
        browserWsPath = await resolveBrowserWsPath();
      } catch (e) {
        console.error("hiver chrome CDP host: resolve browser ws failed:", e.message);
      }
      // Relay first (the warmup attaches through it), then warm + open a page with
      // Playwright before the snapshot, so a resumed sandbox is already warm on the
      // first client attach.
      relay.listen(PORT, "0.0.0.0", async () => {
        await warmup();
        // Readiness is written once the relay is up and the CDP path is warm
        // (matches the old browser-host.cjs, which wrote it from its 0.0.0.0 listen
        // callback). Under microvm isolation sbxguest waits for this file before
        // snapshotting the warm VM.
        fs.mkdirSync(path.dirname(READY_FILE), { recursive: true });
        fs.writeFileSync(READY_FILE, String(child.pid));
        console.log(
          `hiver chrome CDP host: relay 0.0.0.0:${PORT}${STABLE_PATH} -> 127.0.0.1:${CHROME_CDP_PORT}${browserWsPath || "/devtools/browser/<uuid>"}; browser ready`,
        );
      });
    })();
  }
});

// Forward termination so a stopped container/VM cleanly closes Chrome.
for (const sig of ["SIGTERM", "SIGINT"]) {
  process.on(sig, () => child.kill(sig));
}
child.on("exit", (code, signal) => {
  process.exit(signal ? 1 : code ?? 0);
});
child.on("error", (e) => {
  console.error("hiver chrome CDP host: failed to launch chromium:", e);
  process.exit(1);
});
