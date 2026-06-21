// Resident browser REPL.
//
// This is the image entrypoint, so it runs during the prewarm boot and stays
// resident — every sandbox claimed from the warm pool inherits Node, Playwright,
// and Chromium already loaded and running (captured in the microvm snapshot, or
// kept alive in the runc container). It removes the three stacked costs of
// `node -e "require('playwright'); chromium.launch()"` per exec: node startup,
// require('playwright') (~0.9s), and chromium.launch() (~1.1s).
//
// Transport: HTTP. A `POST /eval` endpoint, reachable through the sandbox ingress
// proxy (/v1/<key>/proxy/<port>/eval), drives the warm browser — the request body
// is the JS command and the response body is its console output. No exec session
// and no per-request in-guest process spawn (the previous design bridged a Unix
// socket with `socat` over execStream). A single, process-wide REPL backs the
// endpoint, so top-level bindings (const/let, helpers) persist across requests:
// `const a = 1` in one request, `a` in the next. Evals are serialized — one
// shared REPL is not concurrency-safe.
//
// The shared warm `browser`/`context`/`page` are seeded into the session (the
// `page` is pre-opened at prewarm, so it's usable immediately) and must NOT be
// closed by command code. `console` is bound per request so command output is
// captured back into the response.
//
// Readiness signal: once Chromium is launched and the endpoint is listening, this
// writes READY_FILE (/run/hiver/prewarm-ready). Under microvm isolation sbxguest
// waits for that file before letting the host snapshot the (now warm) VM. Under
// runc isolation the file is unused — container readiness is the poststart fifo.
const http = require("http");
const fs = require("fs");
const path = require("path");
const util = require("util");
const repl = require("repl");
const { Readable, Writable } = require("stream");
const { chromium } = require("playwright");

// TCP port the HTTP /eval endpoint listens on, reached via the sandbox ingress
// proxy (/v1/<key>/proxy/<port>/eval). Bound on 0.0.0.0 so the proxy can dial
// the guest at its IP. Keep in sync with the client default.
const PORT = Number(process.env.HIVER_BROWSER_PORT || "9223");
const READY_FILE = process.env.HIVER_PREWARM_READY_FILE || "/run/hiver/prewarm-ready";
// Profile baked into the image layer (see Dockerfile/prewarm.cjs).
// launchPersistentContext reuses it so the prewarm launch skips first-run
// profile creation; chromium.launch() would ignore it.
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
const chromiumArgs = [
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

(async () => {
  // The prewarm hook runs as the guest's root init. Persistent context reuses
  // the baked profile. `context` is the warm default context; `browser` (its
  // parent) is exposed too for isolated context.newContext(). Both are shared
  // across sessions and must NOT be closed by command code. See chromiumArgs
  // above for the cost-minimizing launch flags.
  const context = await chromium.launchPersistentContext(USER_DATA_DIR, {
    headless: true,
    args: chromiumArgs,
  });
  const browser = context.browser();
  // Pre-open a page here, at prewarm, so it's captured warm in the snapshot and
  // sessions get an immediately-usable `page` without paying browser.newPage()
  // (renderer spawn) on the request path. Shared across sessions like
  // browser/context; commands that need isolation can still browser.newPage().
  const page = await context.newPage();

  // One process-wide persistent REPL behind POST /eval. Created once (not per
  // request), so bindings persist across HTTP requests — the whole sandbox is the
  // session. Input is never fed (we drive eval directly via runEval); a Readable
  // that never pushes keeps the readline interface idle. Output is a discard sink
  // — command output is captured via the per-eval `console` binding below.
  const r = repl.start({
    input: new Readable({ read() {} }),
    output: new Writable({ write(_c, _e, cb) { cb(); } }),
    terminal: false,
    prompt: "",
    useColors: false,
    ignoreUndefined: true,
    writer: () => "",
  });
  // Seed the shared warm handles; commands run against these.
  r.context.browser = browser;
  r.context.context = context;
  r.context.page = page;

  // Run one command, capturing its console output into the returned string. The
  // REPL evaluates in r.context (top-level await on by default), so bindings
  // persist into later calls.
  const evalOnce = (cmd) =>
    new Promise((resolve) => {
      let out = "";
      r.context.console = {
        log: (...a) => {
          out += util.format(...a) + "\n";
        },
        error: (...a) => {
          out += util.format(...a) + "\n";
        },
      };
      const code = cmd.endsWith("\n") ? cmd : cmd + "\n";
      r.eval(code, r.context, "repl", (err, _result) => {
        // No multi-line buffering over HTTP: an incomplete command is an error.
        if (err) out += String((err && err.stack) || err) + "\n";
        resolve(out);
      });
    });

  // Serialize evals: a single REPL is not concurrency-safe (interleaved evals
  // would corrupt state and output routing). Chain each request behind the last.
  let evalChain = Promise.resolve();
  const runEval = (cmd) => {
    const result = evalChain.then(() => evalOnce(cmd));
    evalChain = result.then(
      () => {},
      () => {},
    ); // keep the chain alive regardless of outcome
    return result;
  };

  const server = http.createServer((req, res) => {
    if (req.method === "GET") {
      // Health/attach check — 200 once the browser host is up.
      res.writeHead(200, { "content-type": "text/plain" });
      res.end("ok\n");
      return;
    }
    if (req.method !== "POST" || !req.url.startsWith("/eval")) {
      res.writeHead(404, { "content-type": "text/plain" });
      res.end("not found\n");
      return;
    }
    let body = "";
    req.setEncoding("utf8");
    req.on("data", (c) => {
      body += c;
    });
    req.on("end", async () => {
      try {
        const out = await runEval(body);
        res.writeHead(200, { "content-type": "text/plain" });
        res.end(out);
      } catch (e) {
        res.writeHead(500, { "content-type": "text/plain" });
        res.end(String((e && e.stack) || e) + "\n");
      }
    });
    req.on("error", () => {});
  });

  server.listen(PORT, "0.0.0.0", () => {
    fs.mkdirSync(path.dirname(READY_FILE), { recursive: true });
    fs.writeFileSync(READY_FILE, String(process.pid));
    console.log(`hiver browser host: http :${PORT}; browser ready`);
  });
})().catch((e) => {
  console.error("hiver browser host: fatal:", e);
  process.exit(1);
});
