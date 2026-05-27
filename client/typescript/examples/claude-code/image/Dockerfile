FROM node:22-bookworm-slim

ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
        openssh-server \
    && ARCH=$(dpkg --print-architecture) \
    && case "$ARCH" in \
         amd64) ZELLIJ_ARCH="x86_64-unknown-linux-musl" ;; \
         arm64) ZELLIJ_ARCH="aarch64-unknown-linux-musl" ;; \
       esac \
    && curl -fkL "https://github.com/zellij-org/zellij/releases/latest/download/zellij-${ZELLIJ_ARCH}.tar.gz" | tar xz -C /usr/local/bin \
    && rm -rf /var/lib/apt/lists/* \
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication yes/' /etc/ssh/sshd_config \
    && sed -i 's/UsePAM yes/UsePAM no/' /etc/ssh/sshd_config \
    && echo 'PermitUserEnvironment yes' >> /etc/ssh/sshd_config \
    && echo 'ForceCommand zellij attach --create claude' >> /etc/ssh/sshd_config \
    && useradd -m -s /bin/bash agent \
    && echo 'agent:agent' | chpasswd \
    && echo 'agent ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers

RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli @github/copilot

RUN mkdir -p /home/agent/.claude
COPY settings.json /home/agent/.claude/settings.json
RUN chown -R agent:agent /home/agent/.claude

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 22

ENTRYPOINT ["/entrypoint.sh"]
