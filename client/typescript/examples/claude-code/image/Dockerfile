FROM node:22-bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json index.ts ./

EXPOSE 3000

ENTRYPOINT ["npx", "tsx", "index.ts"]
