FROM hiversh/agent-base-standalone

# Tell Claude Code it's already sandboxed so --dangerously-skip-permissions
# is allowed despite the uid-0 check.
ENV IS_SANDBOX=1

RUN npm install -g @anthropic-ai/claude-code

# The memory dir is mounted directly as GCS (see lib/hiver/gcs.ts memoryFs); its
# mount point must exist in the image, since the enclosing projects dir is no
# longer a mount that would provide it. Its parent (…/projects/-workspace) also
# holds the ephemeral transcript the agent writes at runtime.
RUN mkdir -p /home/agent/.claude/hooks /home/agent/.claude/skills \
    /home/agent/.claude/projects/-workspace/memory
COPY .claude/settings.json /home/agent/.claude/settings.json
# Skills (with deps) come from agent-base at /home/agent/skills; copy them into
# the Claude CLI's skill dir so it can discover them.
RUN cp -a /home/agent/skills/. /home/agent/.claude/skills/
COPY .claude.json /home/agent/.claude.json
RUN chown -R agent:agent /home/agent/.claude \
    && chown agent:agent /home/agent/.claude.json

