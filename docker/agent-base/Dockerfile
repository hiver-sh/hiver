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
        socat \
        procps \
    && rm -rf /var/lib/apt/lists/* \
    # Everything runs as root (uid/gid 0) — just relabeled to "agent" with a tidy
    # home at /home/agent instead of /root. usermod/groupmod refuse to touch the
    # live uid-0 user during build, so edit the passwd/group databases directly.
    && sed -i 's#^root:x:0:0:root:/root:#agent:x:0:0:agent:/home/agent:#' /etc/passwd \
    && sed -i 's/^root:x:0:/agent:x:0:/' /etc/group \
    && mkdir -p /home/agent

# Hiver TypeScript client, available globally for agents. Installed from the
# freshly-built local package (packed into this build context by the Makefile's
# `stage-agent-base-client`), so the image always ships the current client
# instead of a pinned npm release. The global install pulls the client's runtime
# deps (zod) alongside it. The browser skill then references this same global
# install directory (see its package.json), so it never re-fetches from npm.
COPY hiver-client.tgz /opt/hiver-client.tgz
RUN npm install -g /opt/hiver-client.tgz

# Generic agent skills. Kept out of any agent-specific location so the base
# stays generic; individual agent images copy these into their own skill dir.
# --install-links copies the local @hiver.sh/client dep into node_modules
# instead of symlinking it: a symlink would be relative (computed from this
# path's depth) and dangle once an agent image (e.g. claude/) cp -a's this
# tree somewhere else at a different depth, like /home/agent/.claude/skills.
COPY skills /home/agent/skills
RUN cd /home/agent/skills/browser/scripts && npm install --install-links

ENTRYPOINT ["tail", "-f", "/dev/null"]
