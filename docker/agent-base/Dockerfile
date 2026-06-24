FROM node:22-bookworm-slim

ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8
ENV HOME=/home/agent

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

ENTRYPOINT ["tail", "-f", "/dev/null"]
