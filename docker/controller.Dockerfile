FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
# go.mod replaces github.com/hiver-sh/hiver/client with the local ./client/go
# module, so `go mod download` must be able to read its go.mod. Copy the
# replaced module's manifest before resolving (the full sources arrive with
# `COPY . .` below).
COPY client/go/go.mod client/go/go.sum ./client/go/
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/controller ./cmd/controller

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        docker.io \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/controller /usr/local/bin/controller

EXPOSE 9000
ENTRYPOINT ["/usr/local/bin/controller"]
