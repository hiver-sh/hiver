FROM node:22-bookworm-slim

ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
        openssh-server \
        tmux \
    && rm -rf /var/lib/apt/lists/* \
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication yes/' /etc/ssh/sshd_config \
    && sed -i 's/UsePAM yes/UsePAM no/' /etc/ssh/sshd_config \
    && echo 'PermitUserEnvironment yes' >> /etc/ssh/sshd_config \
    && echo 'ForceCommand tmux -u attach-session -t claude' >> /etc/ssh/sshd_config \
    && useradd -m -s /bin/bash claude-agent \
    && echo 'claude-agent:root' | chpasswd \
    && echo 'claude-agent ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers \
    && printf 'set -g default-terminal "xterm-256color"\nset -ga terminal-overrides ",xterm-256color:RGB"\nset-option -g window-size latest\nset-environment -g COLORTERM truecolor\nset-environment -g LANG C.UTF-8\nset -g mouse on\n' > /etc/tmux.conf

RUN npm install -g @anthropic-ai/claude-code

RUN mkdir -p /home/claude-agent/.claude
COPY settings.json /home/claude-agent/.claude/settings.json
RUN chown -R claude-agent:claude-agent /home/claude-agent/.claude
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 22

ENTRYPOINT ["/entrypoint.sh"]
