// Resident Chrome, driven over CDP.
//
// This is the image entrypoint, so it runs during the prewarm boot and stays
// resident — every sandbox claimed from the warm pool inherits Chromium already
// launched and listening (captured in the microvm snapshot, or kept alive in the
// runc container).
//
// Unlike the old browser-host.cjs, *no Playwright runs in this container*. We
// just spawn the Playwright-managed Chromium binary directly with its DevTools
// (CDP) endpoint open and supervise it. An external Playwright host attaches with
//
//     const browser = await chromium.connectOverCDP("http://<host>:9223")
//
// reached through the sandbox ingress proxy (/v1/<key>/proxy/9223). connectOverCDP
// speaks raw CDP over WebSocket (HTTP is used only for the /json/version
// discovery handshake), so the attaching host can run any Playwright version — it
// does not have to match the Chromium baked into this image.
//
// Transport recap: Chrome exposes an HTTP discovery server on the debugging port
// (GET /json/version → webSocketDebuggerUrl) and then all CDP commands/events
// flow over a WebSocket. We bind it on 0.0.0.0 so the proxy can dial the guest at
// its IP.
//
// Readiness signal: Chrome prints "DevTools listening on ws://..." to stderr once
// the endpoint is up; we write READY_FILE then. Under microvm isolation sbxguest
// waits for that file before letting the host snapshot the (now warm) VM. Under
// runc isolation the file is unused — container readiness is the poststart fifo.
const { spawn } = require("child_process");
const fs = require("fs");
const path = require("path");
const { chromium } = require("playwright");

// TCP port the CDP/DevTools endpoint listens on, reached via the sandbox ingress
// proxy (/v1/<key>/proxy/<port>). Keep in sync with the client default.
const PORT = Number(process.env.HIVER_BROWSER_PORT || "9223");
const READY_FILE = process.env.HIVER_PREWARM_READY_FILE || "/run/hiver/prewarm-ready";
// Profile baked into the image layer (see Dockerfile/prewarm.cjs). Reusing it
// skips first-run profile creation (Local State, Preferences, etc.) at runtime.
const USER_DATA_DIR =
  process.env.PLAYWRIGHT_CHROMIUM_USER_DATA_DIR || "/usr/local/ms-playwright-profile";

// Chromium launch flags tuned to run as cheaply as possible. The resident host
// keeps one browser alive per sandbox, so its idle footprint (memory of the
// always-on processes) and per-navigation CPU set how many browsers a node can
// pack — i.e. the cost per session. So we collapse the process model to a single
// process (no renderer/zygote forks — lowest possible memory), drop the
// GPU/SwiftShader path (no paint is needed for DOMContentLoaded automation),
// disable image decode, shrink every internal cache, and strip background
// subsystems.
//
// NOTE: --single-process headless is not officially supported by Chromium and
// can crash on some heavy/complex pages. We accept that here because this host
// drives simple navigation/automation workloads where the memory savings (one
// process instead of browser+zygote+gpu+renderer) dominate. If a workload starts
// crashing, dropping --single-process/--no-zygote is the first thing to revert.
const args = [
  // --- DevTools/CDP endpoint ---
  // Bind on all interfaces so the ingress proxy can dial the guest at its IP.
  // --remote-allow-origins=* is required by Chrome 111+: without it Chrome
  // rejects the WebSocket upgrade with HTTP 403 on its origin check, which is
  // exactly the connectOverCDP handshake.
  `--remote-debugging-port=${PORT}`,
  "--remote-debugging-address=0.0.0.0",
  "--remote-allow-origins=*",
  // headless + reuse the baked profile so there's no first-run cost at runtime.
  "--headless=new",
  `--user-data-dir=${USER_DATA_DIR}`,

  // --- cost-minimizing flags (mirror prewarm.cjs / the old browser-host.cjs) ---
  // the prewarm hook runs as the guest's root init, so the sandbox is required.
  "--no-sandbox",
  // one process for everything: no zygote pre-fork, no separate renderer/GPU
  // processes — the lowest possible resident memory.
  "--single-process",
  "--no-zygote",
  // collapse the process model further (belt-and-suspenders if single-process is
  // ever reverted): one renderer reused across (cross-origin) navigations.
  "--disable-features=site-per-process,IsolateOrigins,Translate,BackForwardCache,MediaRouter,OptimizationHints,AcceptCHFrame,AudioServiceOutOfProcess",
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
  // strip background CPU / network chatter.
  "--disable-background-networking",
  "--disable-component-update",
  "--disable-sync",
  "--disable-default-apps",
  "--disable-extensions",
  "--metrics-recording-only",
  "--disable-breakpad",
  "--no-first-run",
  "--no-default-browser-check",
  "--mute-audio",
  "--disable-hang-monitor",
  "--disable-client-side-phishing-detection",
  "--disable-ipc-flooding-protection",
  // a 1 GiB guest's default /dev/shm is tiny; use /tmp instead.
  "--disable-dev-shm-usage",
];

// Spawn the raw Chromium binary that `npx playwright install` placed under
// PLAYWRIGHT_BROWSERS_PATH. executablePath() resolves it; nothing else here
// touches Playwright at runtime.
const child = spawn(chromium.executablePath(), args, {
  stdio: ["ignore", "inherit", "pipe"],
});

let ready = false;
child.stderr.on("data", (buf) => {
  const s = buf.toString();
  process.stderr.write(s);
  // Chrome prints e.g. "DevTools listening on ws://0.0.0.0:9223/devtools/browser/<id>"
  // once the CDP endpoint is accepting connections.
  if (!ready && /DevTools listening on ws:\/\//.test(s)) {
    ready = true;
    fs.mkdirSync(path.dirname(READY_FILE), { recursive: true });
    fs.writeFileSync(READY_FILE, String(child.pid));
    console.log(`hiver chrome CDP host: listening :${PORT}; browser ready`);
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
