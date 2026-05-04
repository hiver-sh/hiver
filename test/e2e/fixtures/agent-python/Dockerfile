FROM python:3.12-slim

# curl is here so commands sent through the agent's /exec endpoint can
# exercise outbound HTTP/HTTPS through the proxy (e.g.
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

COPY agent.py /agent.py

ENTRYPOINT ["python3", "/agent.py"]
