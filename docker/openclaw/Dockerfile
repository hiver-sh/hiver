FROM hiversh/agent-base-standalone

# tini as PID 1. OpenClaw expects to run under an init (systemd / `docker run
# --init`); when it is PID 1 itself (node), orphaned child processes that
# reparent to PID 1 are never reaped and pile up as <defunct> zombies (one per
# heartbeat, ~every 5 min). tini forwards signals and reaps those orphans.
RUN apt-get update \
    && apt-get install -y --no-install-recommends tini \
    && rm -rf /var/lib/apt/lists/* \
    && test -x /usr/bin/tini

RUN npm install -g openclaw@latest

# Bake a minimal config so `gateway run` (and `tui`) start without setup.
#  - gateway.mode=local: otherwise the gateway exits with "Missing config. Run
#    `openclaw setup` or set gateway.mode=local".
# Let openclaw write the file itself (~/.openclaw/openclaw.json) so the format
# stays whatever the installed version expects.
RUN openclaw config set gateway.mode local

# Hiver sandbox backend plugin. It registers a "hiver" sandbox backend so the
# gateway routes exec/process and the filesystem tools (read/write/edit/
# apply_patch) into a nested Hiver sandbox instead of running them on the
# gateway host. Install its own deps (the @hiver.sh/client the plugin imports),
# then register it with OpenClaw's plugin loader.
COPY hiver-sandbox /opt/hiver-sandbox
RUN cd /opt/hiver-sandbox && npm install --omit=dev \
    && openclaw plugins install /opt/hiver-sandbox \
    && openclaw plugins inspect hiver-sandbox --runtime

# Turn sandboxing on and select the Hiver backend. `mode=all` sandboxes every
# session; every tool that can touch the filesystem or spawn a process now runs
# inside the Hiver sandbox.
RUN openclaw config set agents.defaults.sandbox.mode all \
    && openclaw config set agents.defaults.sandbox.backend hiver

# Browser: swap OpenClaw's built-in browser for the agent-base CDP browser skill.
#  - Disable the bundled `browser` plugin. It ships enabled by default and
#    contributes both the `browser` tool and its `browser-automation` skill, both
#    of which drive OpenClaw's own browser engine on the gateway host — not the
#    Chrome inside our Hiver sandbox. Turning the plugin off drops the tool and
#    the skill. entrypoint.sh re-asserts this because provisioning injects its own
#    openclaw.json that would otherwise re-enable it.
#  - Install agent-base's CDP browser skill into OpenClaw's managed skill dir
#    (~/.openclaw/skills, which the gateway scans) so the agent gets a browser
#    that talks to the sandbox's headless Chrome over CDP instead. The skill's
#    helper scripts (with deps) come from agent-base at /home/agent/skills; copy
#    the whole dir so it is self-contained, then rewrite the script paths baked
#    into SKILL.md — they point at the Claude CLI's ~/.claude/skills layout — to
#    this copy's ~/.openclaw/skills location.
RUN openclaw config set plugins.entries.browser.enabled false \
    && mkdir -p /home/agent/.openclaw/skills \
    && cp -a /home/agent/skills/browser /home/agent/.openclaw/skills/browser \
    && sed -i 's#/home/agent/.claude/skills/#/home/agent/.openclaw/skills/#g' \
        /home/agent/.openclaw/skills/browser/SKILL.md

# Skip the Control UI / webchat device-pairing gate. Without an owner UI to
# approve new browser devices, the gateway rejects every connect with "device
# pairing required"; this makes it rely on the shared password (see
# `--auth password` on the entrypoint's gateway run) alone. entrypoint.sh
# re-asserts this on every start because provisioning injects its own
# openclaw.json that would otherwise drop it.
RUN openclaw config set gateway.controlUi.dangerouslyDisableDeviceAuth true

# REQUIRED at runtime: HIVER_GATEWAY_URL — the address of the Hiver control
# plane the backend provisions nested sandboxes from. There is no Hiver gateway
# inside this container, so the client default (http://localhost:10000) will NOT
# work; pass a URL reachable from here at `docker run` (or in the sandbox
# config), e.g. `-e HIVER_GATEWAY_URL=http://hiver-gateway:10000`. When it is
# unreachable the backend logs "sandbox provision FAILED" and the sandboxed tool
# call errors (it does not silently fall back to the host).
#
# Optional at runtime: HIVER_SANDBOX_IMAGE — the logical Hiver image the nested
# sandbox boots (defaults to "openclaw").
#
# Left unset here on purpose: an empty ENV value is NOT the same as unset for URL
# resolution, so baking `HIVER_GATEWAY_URL=""` would break the client default.

# The gateway password. openclaw's `--password` defaults to
# OPENCLAW_GATEWAY_PASSWORD, so baking it here makes the value discoverable and
# overridable at `docker run` (or sandbox config) time without editing the
# entrypoint. Clients must send it as connect.params.auth.password.
ENV OPENCLAW_GATEWAY_PASSWORD=hiver-openclaw

EXPOSE 18789

# The gateway is launched through entrypoint.sh, which re-asserts the Hiver
# sandbox config on every start (provisioning injects its own openclaw.json that
# would otherwise drop it) and clears a stale attestation over an empty
# workspace, then execs the gateway. Flags on the exec line:
# --bind lan: bind the sandbox network interface, not just loopback. The ingress
# proxy reaches the workload over the guest's network IP (not 127.0.0.1), so the
# default loopback bind makes the gateway unreachable through the /proxy/ route
# (connection refused → 502).
# --auth password: openclaw refuses to bind beyond loopback without a shared
# secret, so a password is required to expose the gateway on the sandbox
# interface. The password resolves from OPENCLAW_GATEWAY_PASSWORD (set above).
# Token and every other auth mode stay off; only this shared password is accepted.
COPY entrypoint.sh /usr/local/bin/openclaw-entrypoint.sh
RUN chmod +x /usr/local/bin/openclaw-entrypoint.sh
# tini (PID 1) → entrypoint.sh → exec openclaw. tini stays PID 1 and reaps
# reparented orphans; openclaw runs as its child.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/openclaw-entrypoint.sh"]
