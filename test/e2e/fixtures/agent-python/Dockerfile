FROM python:3.12-slim

# curl is here so commands sent through the agent's /exec endpoint can
# exercise outbound HTTP/HTTPS through the proxy (e.g.
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

COPY agent.py /agent.py

# Initial workspace content. sandboxd moves anything at /workspace
# in this image's rootfs into the FUSE backend before mounting, so
# the agent finds these files at /workspace/inputs/* with whatever
# access the spec.yaml ACLs grant for that path (here: ro).
COPY inputs/ /workspace/inputs/

ENTRYPOINT ["python3", "/agent.py"]
