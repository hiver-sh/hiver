// Resident browser REPL.
//
// This is the image entrypoint, so it runs during the prewarm boot and stays
// resident — every sandbox claimed from the warm pool inherits Node, Playwright,
// and Chromium already loaded and running (captured in the microvm snapshot, or
// kept alive in the runc container). Clients drive the live browser over a single
// `execStream` session bridged to this process's Unix socket (see
// examples/benchmark-browser-resident.ts), which removes the three stacked costs
// of `node -e "require('playwright'); chromium.launch()"` per exec: node startup,
// require('playwright') (~0.9s), and chromium.launch() (~1.1s).
//
// Transport: a Unix domain socket. One connection == one *stateful* session: a
// Node REPL is bound to the socket, so top-level bindings (const/let, helpers)
// persist across commands within the session — open a page in one command and
// act on it in the next. The shared warm `browser`/`context`/`page` are seeded
// into the session (the `page` is pre-opened at prewarm, so it's usable
// immediately) and must NOT be closed by command code. `console` is bound to
// the socket so command output streams back.
//
// Line protocol: the client sends one JS command per line; after each one
// completes the host writes a line `__READY__` (and one on connect), so the
// driver knows when to send the next — same protocol as
// examples/playwright-exec-stream.ts.
//
// Readiness signal: once Chromium is launched and the socket is listening, this
// writes READY_FILE (/run/hiver/prewarm-ready). Under microvm isolation sbxguest
// waits for that file before letting the host snapshot the (now warm) VM. Under
// runc isolation the file is unused — container readiness is the poststart fifo.
const net = require("net");
const fs = require("fs");
const path = require("path");
const util = require("util");
const repl = require("repl");
const { chromium } = require("playwright");

const SOCK = process.env.HIVER_BROWSER_SOCK || "/run/hiver/browser.sock";
const READY_FILE = process.env.HIVER_PREWARM_READY_FILE || "/run/hiver/prewarm-ready";
// Profile baked into the image layer (see Dockerfile/prewarm.cjs).
// launchPersistentContext reuses it so the prewarm launch skips first-run
// profile creation; chromium.launch() would ignore it.
const USER_DATA_DIR =
  process.env.PLAYWRIGHT_CHROMIUM_USER_DATA_DIR || "/usr/local/ms-playwright-profile";
const MARKER = "__READY__";

(async () => {
  // --no-sandbox: the prewarm hook runs as the guest's root init. Persistent
  // context reuses the baked profile. `context` is the warm default context;
  // `browser` (its parent) is exposed too for isolated context.newContext().
  // Both are shared across sessions and must NOT be closed by command code.
  const context = await chromium.launchPersistentContext(USER_DATA_DIR, {
    headless: true,
    args: ["--no-sandbox"],
  });
  const browser = context.browser();
  // Pre-open a page here, at prewarm, so it's captured warm in the snapshot and
  // sessions get an immediately-usable `page` without paying browser.newPage()
  // (renderer spawn) on the request path. Shared across sessions like
  // browser/context; commands that need isolation can still browser.newPage().
  const page = await context.newPage();

  const server = net.createServer((sock) => {
    sock.setNoDelay(true);

    // A REPL bound to the socket gives a genuinely stateful session: bindings
    // declared in one command survive into the next. terminal:false + empty
    // prompt + a no-op writer keep the wire clean (commands emit their own
    // output via console); top-level await is on by default.
    const r = repl.start({
      input: sock,
      output: sock,
      terminal: false,
      prompt: "",
      useColors: false,
      ignoreUndefined: true,
      writer: () => "",
    });

    // Seed the session: shared warm browser/context, and a console that streams
    // to this socket (commands written like playwright-exec-stream.ts work).
    r.context.browser = browser;
    r.context.context = context;
    r.context.page = page;
    r.context.console = {
      log: (...a) => sock.write(util.format(...a) + "\n"),
      error: (...a) => sock.write(util.format(...a) + "\n"),
    };

    // Emit __READY__ after each command settles (including awaited promises) so
    // the driver knows the session is idle. Recoverable (incomplete input) is
    // passed through untouched so multi-line entry still buffers.
    const defaultEval = r.eval;
    r.eval = function (cmd, ctx, file, cb) {
      defaultEval.call(this, cmd, ctx, file, (err, result) => {
        if (err && err instanceof repl.Recoverable) {
          cb(err);
          return;
        }
        if (err) sock.write(String((err && err.stack) || err) + "\n");
        sock.write(MARKER + "\n");
        cb(null, result);
      });
    };

    r.on("exit", () => sock.end());
    sock.on("error", () => {}); // client disconnects are normal; don't crash.
    sock.write(MARKER + "\n"); // session live
  });

  fs.mkdirSync(path.dirname(SOCK), { recursive: true });
  try {
    fs.unlinkSync(SOCK); // clear a stale socket from a previous boot.
  } catch {
    /* not present */
  }
  server.listen(SOCK, () => {
    fs.mkdirSync(path.dirname(READY_FILE), { recursive: true });
    fs.writeFileSync(READY_FILE, String(process.pid));
    console.log(`hiver browser host: listening on ${SOCK}; browser ready`);
  });
})().catch((e) => {
  console.error("hiver browser host: fatal:", e);
  process.exit(1);
});
