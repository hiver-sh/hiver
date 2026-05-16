FROM node:20-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        curl \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY app.js /app.js

COPY inputs/ /workspace/inputs/

EXPOSE 8081

ENTRYPOINT ["node", "/app.js"]
