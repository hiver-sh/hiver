const { chromium } = require('playwright');

(async () => {
  const ctx = await chromium.launchPersistentContext(
    process.env.PLAYWRIGHT_CHROMIUM_USER_DATA_DIR,
    {
      headless: true,
      // --no-sandbox: prewarm runs as root during the image build.
      // --single-process / --disable-gpu: when the foreign-arch image is built
      // under QEMU emulation (e.g. amd64 on an arm64 host), Chromium's separate
      // GPU subprocess segfaults ("GPU process isn't usable. Goodbye."), which
      // aborts the whole multi-arch build. Running single-process avoids
      // spawning that subprocess. This only affects the one-off prewarm launch.
      args: ['--no-sandbox', '--disable-gpu', '--single-process'],
    }
  );
  await ctx.newPage();
  await ctx.close();
})();
