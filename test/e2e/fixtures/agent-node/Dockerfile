FROM node:20-slim

# curl is used by the agent to exercise outbound HTTPS via a different
# TLS stack than Node's built-in tls module — same /exec endpoint
# semantics as agent-python.
RUN apt-get update && apt-get install -y --no-install-recommends \
        curl \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY agent.js /agent.js

# Initial workspace content. sandboxd moves anything at /workspace
# in this image's rootfs into the FUSE backend before mounting, so
# the agent finds these files at /workspace/inputs/* with whatever
# access the spec.yaml ACLs grant for that path (here: ro).
COPY inputs/ /workspace/inputs/

ENTRYPOINT ["node", "/agent.js"]
