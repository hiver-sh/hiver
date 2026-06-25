FROM agent-base

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
