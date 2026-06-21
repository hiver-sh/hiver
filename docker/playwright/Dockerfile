FROM node:20-bookworm-slim

# PLAYWRIGHT_BROWSERS_PATH moves the browser cache out of the root home dir
# so it's accessible regardless of the workload's uid.
ENV PLAYWRIGHT_BROWSERS_PATH=/usr/local/ms-playwright
# Makes globally-installed npm packages (playwright, etc.) available to any
# node process without NODE_PATH being set manually by the caller.
ENV NODE_PATH=/usr/local/lib/node_modules

RUN apt-get update \
    && npm install -g playwright \
    && npx playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*

# Pre-warm Chromium so the first in-sandbox launch is fast:
#   fc-cache: builds the fontconfig cache (Chromium scans fonts on first run).
#   prewarm.cjs launches Chromium with a fixed userDataDir so the default
#   profile (Local State, Preferences, etc.) is baked into the image layer and
#   doesn't need to be created at runtime.
ENV PLAYWRIGHT_CHROMIUM_USER_DATA_DIR=/usr/local/ms-playwright-profile
COPY prewarm.cjs /tmp/prewarm.cjs
RUN fc-cache -f \
    && node /tmp/prewarm.cjs \
    && rm /tmp/prewarm.cjs

# Resident browser host: this is the image entrypoint, so it runs during the
# prewarm boot and stays resident — a claimed sandbox inherits Chromium already
# running (captured in the microvm snapshot, or kept alive in the runc container).
# Clients drive it over HTTP (POST /eval through the sandbox ingress proxy),
# instead of paying node startup + require('playwright') + chromium.launch() per
# exec. The required env (NODE_PATH, PLAYWRIGHT_*) is set via ENV above. See
# browser-host.cjs.
COPY browser-host.cjs /opt/hiver/browser-host.cjs

EXPOSE 9223

ENTRYPOINT ["node", "/opt/hiver/browser-host.cjs"]
