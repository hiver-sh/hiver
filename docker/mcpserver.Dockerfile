FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/mcpserver ./cmd/mcpserver

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        jq \
        nodejs \
        npm \
        python3 \
        python3-pip \
        python3-venv \
    && rm -rf /var/lib/apt/lists/*

ENV PIP_BREAK_SYSTEM_PACKAGES=1

RUN npm install -g playwright \
    && playwright install --with-deps chromium

COPY --from=build /out/mcpserver /usr/local/bin/mcpserver

EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/mcpserver"]
