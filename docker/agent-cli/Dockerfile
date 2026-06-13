FROM node:22-bookworm-slim

ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8
ENV HOME=/home/agent
# We run as root (relabeled "agent"); tell Claude Code it's already sandboxed so
# --dangerously-skip-permissions is allowed despite the uid-0 check.
ENV IS_SANDBOX=1

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
        python3 \
        python3-pip \
        jq \
    && rm -rf /var/lib/apt/lists/* \
    # Everything runs as root (uid/gid 0) — just relabeled to "agent" with a tidy
    # home at /home/agent instead of /root. usermod/groupmod refuse to touch the
    # live uid-0 user during build, so edit the passwd/group databases directly.
    && sed -i 's#^root:x:0:0:root:/root:#agent:x:0:0:agent:/home/agent:#' /etc/passwd \
    && sed -i 's/^root:x:0:/agent:x:0:/' /etc/group \
    && mkdir -p /home/agent

RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli @github/copilot

RUN mkdir -p /home/agent/.claude
COPY .claude/settings.json /home/agent/.claude/settings.json
COPY .claude/hooks/notify-hook.js /home/agent/.claude/hooks/notify-hook.js
# Static base config: marks onboarding complete and trusts the project dirs so
# the agent starts without prompts. Auth comes from CLAUDE_CODE_OAUTH_TOKEN in
# the container environment.
COPY .claude.json /home/agent/.claude.json
# Static codex config (model selection).
COPY .codex/config.json /home/agent/.codex/config.json
RUN chmod +x /home/agent/.claude/hooks/notify-hook.js \
    && chown -R agent:agent /home/agent/.claude \
    && chown agent:agent /home/agent/.claude.json \
    && chown -R agent:agent /home/agent/.codex

ENTRYPOINT ["tail", "-f", "/dev/null"]
