import * as hiver from "@hiver.sh/client";
import { chromium } from "playwright-core";

const sandbox = await hiver.getOrCreateSandbox("browser", { image: "browser" });

// Build the CDP WebSocket URL: proxy URL for port 9223, http -> ws, + /cdp
const wsEndpoint = sandbox.proxyUrl(9223).replace(/^http/, "ws") + "cdp";

const browser = await chromium.connectOverCDP(wsEndpoint);
// Reuse the resident browser's warm context and page; fall back to creating
// them only if it came up empty.
const context = browser.contexts()[0] ?? (await browser.newContext());
const page = context.pages()[0] ?? (await context.newPage());

await page.goto("https://news.ycombinator.com", { waitUntil: "domcontentloaded" });
const titles = await page.$$eval(".titleline > a", (els) =>
  els.map((e) => e.textContent),
);
console.log(titles);

await browser.close(); // disconnects the client; the sandbox browser stays warm
