// Drive the sandbox's resident Chromium with Playwright, over CDP.
//
// The browser image launches headless Chromium with its DevTools/CDP endpoint
// open (see docker/browser/chromehost) and keeps it resident. Under microvm
// isolation that warm browser is captured in the snapshot, so a claimed sandbox
// resumes with Chromium already listening.
//
// All the automation logic lives here, in the client: we attach with Playwright's
// chromium.connectOverCDP() through the sandbox ingress proxy and drive the
// browser exactly as if it were local. connectOverCDP speaks raw CDP over a
// WebSocket (HTTP is used only for the /json/version discovery handshake), so the
// client's Playwright version is independent of the Chromium baked into the image.
//
// Run with: npx tsx examples/browser-cdp.ts
import * as hiver from "@hiver.sh/client";
import { chromium } from "playwright-core";

const gatewayUrl = process.env.HIVER_GATEWAY_URL ?? "http://localhost:10000";
// TCP port Chromium's CDP/DevTools endpoint listens on inside the sandbox,
// reached via the ingress proxy. Keep in sync with chromehost.
const CDP_PORT = Number(process.env.HIVER_BROWSER_PORT ?? "9223");

const tStart = performance.now();
const sandbox = await hiver.getOrCreateSandbox(
  "hiver-browser-cdp",
  {
    image: "browser",
    snapshot: {
      vm: {
        key: "browser"
      }
    }
  },
  { gatewayUrl, timeoutMs: 120_000 },
);
console.info(`sandbox created in ${(performance.now() - tStart).toFixed(0)}ms`);

// Attach Playwright over a STABLE CDP url. The in-guest relay (chromehost)
// exposes a fixed /cdp alias and maps it onto Chrome's per-launch
// /devtools/browser/<uuid> endpoint, so there's no /json/version discovery to do
// here. connectOverCDP connects a ws:// url directly (no rewrite), so we just
// point it at the proxy URL + /cdp (http→ws).
const wsEndpoint = sandbox.proxyUrl(CDP_PORT).replace(/^http/, "ws") + "/cdp";

// Retry because the CDP endpoint may not be ready immediately after resume.
async function connectWithRetry(url: string, retries = 5, delayMs = 1000) {
  for (let attempt = 1; attempt <= retries; attempt++) {
    try {
      return await chromium.connectOverCDP(url);
    } catch (err) {
      if (attempt === retries) throw err;
      console.warn(`CDP connect attempt ${attempt} failed, retrying in ${delayMs}ms…`);
      await new Promise((r) => setTimeout(r, delayMs));
    }
  }
  throw new Error("unreachable");
}

const browser = await connectWithRetry(wsEndpoint);
console.info(`connected over CDP in ${(performance.now() - tStart).toFixed(0)}ms`);

const snapshotStart = performance.now();
// await sandbox.snapshot({
//   vm: {
//     key: "browser",
//   },
// });
console.info(`snapshot captured in ${(performance.now() - snapshotStart).toFixed(0)}ms`);


try {
  // Reuse the resident browser's warm context + page rather than creating new
  // ones (a fresh CDP target spins up a new renderer). Fall back to creating them
  // if the resident browser came up empty.
  const context = browser.contexts()[0];
  if (!context) {
    throw new Error('no browser context');
  }
  const page = context.pages()[0]!;
  if (!page) {
    throw new Error('no page');
  }

  for (const url of [
    "https://news.ycombinator.com",
    "https://news.ycombinator.com/news?p=2",
    "https://www.google.com/",
  ]) {
    await page.goto(url, { waitUntil: "domcontentloaded" });
    const titles = await page.$$eval(".titleline > a", (els) =>
      els.map((el) => el.textContent),
    );
    for (const t of titles) console.log(t);
  }
} finally {
  // close() only disconnects this CDP client — it does NOT terminate the resident
  // browser, so the sandbox stays warm for the next attach.
  await browser.close();
}

console.info("\ndone.");
