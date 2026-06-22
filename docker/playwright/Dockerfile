FROM node:20-bookworm-slim

# PLAYWRIGHT_BROWSERS_PATH moves the browser cache out of the root home dir so
# it's accessible regardless of the workload's uid. The browser binary lives here
# independently of the playwright npm package, so it survives the
# `npm uninstall -g playwright` below.
ENV PLAYWRIGHT_BROWSERS_PATH=/usr/local/ms-playwright

# The full `playwright` package is a *build-time* tool: it installs the known-good
# browser binary plus its system library deps, then is removed. A slim
# `playwright-core` is kept in the final image and used at runtime for ONE thing:
# the pre-snapshot CDP warmup in chrome-cdp.cjs (connect over CDP + open a blank
# page so a resumed sandbox starts warm). Clients still drive the browser over CDP
# themselves, independent of this version.
#
# We install chrome-headless-shell, NOT full Chrome. The full browser
# (--headless=new) boots profile/sign-in (GAIA), GCM, the search engine and the
# rest of the background-networking stack, all of which phone home to Google on
# their own. chrome-headless-shell is the standalone successor to old --headless
# and has none of those subsystems compiled in, so it stays silent with no
# anti-telemetry flags. It speaks the same CDP, so chrome-cdp.cjs is unchanged.
RUN apt-get update \
    && npm install -g playwright \
    && npx playwright install --with-deps chromium-headless-shell \
    && rm -rf /var/lib/apt/lists/*

# Build the fontconfig cache (Chromium scans fonts on first run), symlink the
# chrome-headless-shell binary to the stable path chrome-cdp.cjs spawns
# (/opt/hiver/chrome), drop the build-time `playwright`, and install the slim
# `playwright-core` under /opt/hiver so chrome-cdp.cjs can require it at runtime
# for the warmup. (Exec'ing the symlink is fine — Chrome resolves its sibling
# resources via /proc/self/exe, not argv[0].)
RUN fc-cache -f \
    && mkdir -p /opt/hiver \
    && ln -s "$(find "$PLAYWRIGHT_BROWSERS_PATH" \( -name chrome-headless-shell -o -name headless_shell \) -type f | head -1)" /opt/hiver/chrome \
    && npm uninstall -g playwright \
    && npm install --prefix /opt/hiver playwright-core

# Resident Chrome host: this is the image entrypoint, so it runs during the
# prewarm boot and stays resident — a claimed sandbox inherits Chromium already
# launched and listening on the CDP/DevTools port (captured in the microvm
# snapshot, or kept alive in the runc container). Clients attach with Playwright's
# chromium.connectOverCDP() through the sandbox ingress proxy (/v1/<key>/proxy/
# 9223); the only Playwright that runs in this image is the one-shot warmup. See
# chrome-cdp.cjs.
COPY chrome-cdp.cjs /opt/hiver/chrome-cdp.cjs

EXPOSE 9223

ENTRYPOINT ["node", "/opt/hiver/chrome-cdp.cjs"]
