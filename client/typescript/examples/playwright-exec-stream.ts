// Drive Playwright/Chromium inside the sandbox over a single exec stream,
// launching the browser once and reusing it across commands.
//
// We open one long-lived session and feed it commands via `writeStdin`: the
// first command launches Chromium, and every later command reuses that same
// `browser` instance — so the browser startup cost is paid once, not per scrape.
//
// A plain `node -i` REPL doesn't block on top-level `await` between piped lines,
// so a launched-but-not-yet-ready browser would be undefined by the next line.
// Instead we run a tiny stdin evaluator that runs one command at a time against
// a shared `ctx` (where state persists) and prints a `__READY__` marker when
// each finishes — the driver below waits for that marker before sending more.
//
// Run with: npx tsx examples/playwright-exec-stream.ts
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("hiver-playwright-exec-stream", {
  image: "hiversh/playwright",
});

const harness = `
const readline = require('readline');
const ctx = {};
let chain = Promise.resolve();
readline.createInterface({ input: process.stdin }).on('line', (line) => {
  chain = chain.then(async () => {
    if (line.trim()) {
      try {
        await new Function('ctx', 'return (async () => {' + line + '})()')(ctx);
      } catch (e) {
        console.error(e);
      }
    }
    console.log('__READY__');
  });
});
console.log('__READY__');
`;

const exec = await sandbox.execStream(["node", "-e", harness], {
  cwd: "/workspace",
});

// Each command runs in the same session, so `ctx.browser` and `ctx.scrape`
// persist across calls. The browser is launched once, then reused per page.
const commands = [
  // Load the browser once — the expensive step we want to amortize.
  `const { chromium } = require('playwright'); ctx.browser = await chromium.launch()`,
  // A helper that reuses the shared browser to scrape a Hacker News page.
  `ctx.scrape = async (url) => { const page = await ctx.browser.newPage(); await page.goto(url); const titles = await page.$$eval('.titleline > a', els => els.map(el => el.textContent)); await page.close(); for (const t of titles) console.log(t); }`,
  // Reuse the same browser for multiple pages.
  `await ctx.scrape('https://news.ycombinator.com')`,
  `await ctx.scrape('https://news.ycombinator.com/news?p=2')`,
  // Tear down and end the session.
  `await ctx.browser.close(); process.exit(0)`,
];

let next = 0;
const sendNext = () =>
  next < commands.length ? exec.writeStdin(commands[next++] + "\n") : undefined;

// Consume output line by line: a `__READY__` marker means the session is idle
// and ready for the next command; anything else is real output to print.
let buffer = "";
for await (const pipe of exec.pipes) {
  if (pipe.stderr) process.stderr.write(pipe.stderr);
  if (!pipe.stdout) continue;
  buffer += pipe.stdout;
  let nl: number;
  while ((nl = buffer.indexOf("\n")) !== -1) {
    const line = buffer.slice(0, nl);
    buffer = buffer.slice(nl + 1);
    if (line === "__READY__") await sendNext();
    else console.log(line);
  }
}

console.info("\nexit code:", await exec.exitCode);
