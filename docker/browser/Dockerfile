# Resident Chrome host image, driven over CDP.
#
# The entrypoint is a stdlib-only Go binary (chromehost/), the lean replacement
# for the former Node.js chrome-cdp.cjs: it launches chrome-headless-shell with
# its DevTools/CDP endpoint open, fronts it with a 0.0.0.0 relay exposing a stable
# /cdp alias, warms the CDP attach path once before the snapshot, and supervises
# the browser. Nothing Node/Playwright runs at runtime — clients drive the browser
# over CDP themselves (chromium.connectOverCDP), independent of any version here.
#
# We ship chrome-headless-shell, NOT full Chrome: the full browser (--headless=new)
# boots profile/sign-in (GAIA), GCM, the search engine and the rest of the
# background-networking stack, all of which phone home to Google. headless-shell is
# the standalone successor with none of those subsystems compiled in, so it stays
# silent. It speaks the same CDP.

# 1) Build the resident host. Cross-compile on the native BUILDPLATFORM (no QEMU
#    for the Go stage); CGO is off so no cross C toolchain is needed.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS chromehost
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY chromehost/ ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w" -trimpath -o /out/chromehost .

# 2) Fetch the known-good chrome-headless-shell binary. Playwright is a
#    build-time-only tool here (it downloads the browser); nothing from this Node
#    stage ships in the final image.
FROM node:20-bookworm-slim AS browser
ENV PLAYWRIGHT_BROWSERS_PATH=/usr/local/ms-playwright
RUN npm install -g playwright \
    && npx playwright install chromium-headless-shell \
    && rm -rf /var/lib/apt/lists/*

# Rebrand the binary so it stops advertising itself as a bot. chrome-headless-shell
# has the product name "HeadlessChrome" compiled into its .rodata, and that one
# string is the source of BOTH the default User-Agent product token
# ("HeadlessChrome/<ver>") AND the Sec-CH-UA brand ("HeadlessChrome";v=...). The
# brand list is baked into the build — no launch flag or CDP override reaches it
# reliably — so the only real fix is to patch the string in the binary. We rewrite
# "HeadlessChrome" (14 bytes) to "Chrome" + NUL padding (also 14 bytes) so every
# binary offset is preserved; consumers read it as a NUL-terminated C string and
# see "Chrome". Done in this Node stage so only the patched binary (not Node) is
# copied into the final image. Fails the build if the string is gone (e.g. a future
# Chromium bump renamed it) so we can't silently regress to headless branding.
RUN set -eux; \
    bin="$(find "$PLAYWRIGHT_BROWSERS_PATH" \( -name chrome-headless-shell -o -name headless_shell \) -type f | head -1)"; \
    test -n "$bin"; \
    node -e 'const fs=require("fs"),p=process.argv[1],b=fs.readFileSync(p),f=Buffer.from("HeadlessChrome","latin1"),t=Buffer.concat([Buffer.from("Chrome","latin1"),Buffer.alloc(f.length-6)]);let n=0,i=0;while((i=b.indexOf(f,i))!==-1){t.copy(b,i);i+=f.length;n++;}if(!n){console.error("HeadlessChrome not found in "+p);process.exit(1);}fs.writeFileSync(p,b);console.error("rebranded "+n+" occurrence(s) in "+p);' "$bin"

# 3) Final image: the browser, its runtime libs + fonts, and the Go host. No Node,
#    no npm, no Playwright — the only resident process is the static Go binary.
FROM debian:bookworm-slim
ENV PLAYWRIGHT_BROWSERS_PATH=/usr/local/ms-playwright

# chrome-headless-shell runtime shared libraries (mirrors what
# `playwright install-deps chromium-headless-shell` apt-installs on bookworm),
# plus fontconfig + a base font so text shapes. --no-install-recommends keeps it
# lean; ca-certificates is for any TLS the workload's own navigation needs.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates fontconfig fonts-liberation \
        libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 libcairo2 \
        libcups2 libdbus-1-3 libdrm2 libexpat1 libgbm1 libglib2.0-0 libnspr4 \
        libnss3 libpango-1.0-0 libpangocairo-1.0-0 libx11-6 libxcb1 \
        libxcomposite1 libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=browser /usr/local/ms-playwright /usr/local/ms-playwright
COPY --from=chromehost /out/chromehost /opt/hiver/chromehost

# Symlink the chrome-headless-shell binary to the stable path chromehost spawns
# (/opt/hiver/chrome), then build the fontconfig cache (Chromium scans fonts on
# first run). Exec'ing the symlink is fine — Chrome resolves its sibling resources
# via /proc/self/exe, not argv[0].
RUN ln -s "$(find "$PLAYWRIGHT_BROWSERS_PATH" \( -name chrome-headless-shell -o -name headless_shell \) -type f | head -1)" /opt/hiver/chrome \
    && fc-cache -f

EXPOSE 9223

ENTRYPOINT ["/opt/hiver/chromehost"]
