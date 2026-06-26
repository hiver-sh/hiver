FROM hiversh/agent-base-standalone

# Tell Claude Code it's already sandboxed so --dangerously-skip-permissions
# is allowed despite the uid-0 check.
ENV IS_SANDBOX=1

RUN npm install -g @anthropic-ai/claude-code

RUN mkdir -p /home/agent/.claude/hooks
COPY .claude/settings.json /home/agent/.claude/settings.json
COPY .claude/hooks/notify-hook.js /home/agent/.claude/hooks/notify-hook.js
COPY .claude.json /home/agent/.claude.json
RUN chmod +x /home/agent/.claude/hooks/notify-hook.js \
    && chown -R agent:agent /home/agent/.claude \
    && chown agent:agent /home/agent/.claude.json

# Prewarm entrypoint. The microvm prewarm boot runs this as the resident workload,
# then snapshots the guest once /run/hiver/prewarm-ready exists. `claude` is a
# ~215MB Bun-compiled native binary, so a cold first exec page-faults the whole
# thing off the read-only rootfs; running it once here pulls those pages into the
# guest page cache *before* the snapshot, so the snapshot captures them resident
# and the real per-sandbox `claude` (delivered later as the entrypoint override)
# faults almost nothing. Then exec the keepalive so this stays alive as the
# snapshot's resident workload, exactly like agent-base's default entrypoint.
ENTRYPOINT ["/bin/sh", "-c", "claude --version >/dev/null 2>&1 || true; mkdir -p /run/hiver && : > /run/hiver/prewarm-ready; exec tail -f /dev/null"]
