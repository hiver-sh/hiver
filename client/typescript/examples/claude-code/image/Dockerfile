FROM node:22-bookworm-slim

ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
        openssh-server \
        # codex (a crossterm TUI) goes unresponsive after a zellij detach/reattach;
        # running it inside its own tmux session gives it a stable pty that survives
        # the reattach. The other agents don't need it. See entrypoint.sh.
        tmux \
    && ARCH=$(dpkg --print-architecture) \
    && case "$ARCH" in \
         amd64) ZELLIJ_ARCH="x86_64-unknown-linux-musl" ;; \
         arm64) ZELLIJ_ARCH="aarch64-unknown-linux-musl" ;; \
       esac \
    # zellij is the multiplexer the agent runs in (the SSH session attaches to it so the
    # agent survives disconnects). It isn't packaged for Debian, so fetch the binary.
    && curl -fkL "https://github.com/zellij-org/zellij/releases/latest/download/zellij-${ZELLIJ_ARCH}.tar.gz" | tar xz -C /usr/local/bin \
    && rm -rf /var/lib/apt/lists/* \
    # Password auth: the SDK SSHes in as the agent user with the password set below.
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication yes/' /etc/ssh/sshd_config \
    # Disable PAM, which otherwise blocks password logins in this minimal container.
    && sed -i 's/UsePAM yes/UsePAM no/' /etc/ssh/sshd_config \
    # Load ~/.ssh/environment (entrypoint writes the agent token / AGENT / MODEL there)
    # into the SSH session so the agent process inherits them.
    && echo 'PermitUserEnvironment yes' >> /etc/ssh/sshd_config \
    # Every SSH login drops straight into the shared, persistent zellij session.
    && echo 'ForceCommand zellij attach --create claude' >> /etc/ssh/sshd_config \
    && useradd -m -s /bin/bash agent \
    && echo 'agent:agent' | chpasswd \
    # Passwordless sudo so the agent can manage packages/files inside its own sandbox.
    && echo 'agent ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers \
    # truecolor (RGB) passthrough for codex's inner tmux, otherwise it quantizes to 256 colors.
    && printf 'set -g default-terminal "screen-256color"\nset -ga terminal-overrides ",*:RGB"\n' > /etc/tmux.conf

RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli @github/copilot

RUN mkdir -p /home/agent/.claude
COPY .claude/settings.json /home/agent/.claude/settings.json
COPY .claude/hooks/notify-hook.js /home/agent/.claude/hooks/notify-hook.js
# Static base config: marks onboarding complete and trusts the project dirs so
# the agent starts without prompts. Auth comes from CLAUDE_CODE_OAUTH_TOKEN in
# the SSH environment (see entrypoint.sh).
COPY .claude.json /home/agent/.claude.json
# Static codex config (model selection).
COPY .codex/config.json /home/agent/.codex/config.json
RUN chmod +x /home/agent/.claude/hooks/notify-hook.js \
    && chown -R agent:agent /home/agent/.claude \
    && chown agent:agent /home/agent/.claude.json \
    && chown -R agent:agent /home/agent/.codex

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 22

ENTRYPOINT ["/entrypoint.sh"]
