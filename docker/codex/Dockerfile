FROM hiversh/agent-base-standalone

RUN npm install -g @openai/codex

RUN mkdir -p /home/agent/.codex
COPY .codex/config.json /home/agent/.codex/config.json
# Codex reads project trust and approval/sandbox policy from config.toml, not
# config.json: pre-trusts /workspace (skips the "Do you trust the contents of
# this directory?" prompt) and sets approval_policy=never + sandbox_mode=
# danger-full-access (Codex's equivalent of --dangerously-skip-permissions),
# since the Hiver sandbox already provides isolation.
COPY .codex/config.toml /home/agent/.codex/config.toml

# Skills (with deps) come from agent-base at /home/agent/skills; copy the
# browser skill into Codex's skill dir (~/.codex/skills, scanned on startup)
# so it can discover it. The skill's SKILL.md hardcodes script paths for the
# Claude CLI's ~/.claude/skills layout; rewrite them to this copy's location.
RUN mkdir -p /home/agent/.codex/skills \
    && cp -a /home/agent/skills/browser /home/agent/.codex/skills/browser \
    && sed -i 's#/home/agent/.claude/skills/#/home/agent/.codex/skills/#g' \
        /home/agent/.codex/skills/browser/SKILL.md

RUN chown -R agent:agent /home/agent/.codex
