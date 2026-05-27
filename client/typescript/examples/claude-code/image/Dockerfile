FROM node:22-bookworm-slim

ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
        openssh-server \
        tmux \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
         -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
         > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/* \
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication yes/' /etc/ssh/sshd_config \
    && sed -i 's/UsePAM yes/UsePAM no/' /etc/ssh/sshd_config \
    && echo 'PermitUserEnvironment yes' >> /etc/ssh/sshd_config \
    && echo 'ForceCommand tmux -u attach-session -t claude' >> /etc/ssh/sshd_config \
    && useradd -m -s /bin/bash agent \
    && echo 'agent:agent' | chpasswd \
    && echo 'agent ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers \
    && printf 'set -g default-terminal "screen-256color"\nset -ga terminal-overrides ",xterm-256color:RGB"\nset -ga terminal-overrides ",xterm-256color:Tc"\nset -ga terminal-overrides ",*256col*:Tc"\nset-option -g window-size latest\nset-environment -g COLORTERM truecolor\nset-environment -g LANG C.UTF-8\nset -g mouse on\nset -g set-clipboard on\nset -g history-limit 0\n' > /etc/tmux.conf

RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli
RUN su -s /bin/bash agent -c "gh extension install github/gh-copilot"

RUN mkdir -p /home/agent/.claude
COPY settings.json /home/agent/.claude/settings.json
RUN chown -R agent:agent /home/agent/.claude

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 22

ENTRYPOINT ["/entrypoint.sh"]
