FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/mcpserver ./cmd/mcpserver

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/mcpserver /usr/local/bin/mcpserver

EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/mcpserver"]
