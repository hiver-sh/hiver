FROM hiversh/agent-base-standalone

RUN curl -fsSL https://antigravity.google/cli/install.sh | bash \
    && find /home/agent /root /usr/local -name "agy" -type f 2>/dev/null \
       | head -1 | xargs -I{} ln -sf {} /usr/local/bin/agy
