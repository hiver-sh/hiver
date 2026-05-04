# sandbox-runtime — language-agnostic prototype runtime image.
#
# Contains: sandboxd + sbxproxy + sbxfuse, plus the Linux bits the FUSE
# mount needs (fuse3, fusermount). Has *no* agent runtime — Python, Node,
# Go, etc. are added by per-fixture images that use this as their FROM.
#
# Build:  docker build -t sandbox-runtime .
# Use:    test/e2e/fixtures/<lang>/Dockerfile starts with `FROM sandbox-runtime`.
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/sandboxd ./cmd/sandboxd \
 && CGO_ENABLED=0 go build -o /out/sbxproxy ./cmd/sbxproxy \
 && go build -o /out/sbxfuse ./cmd/sbxfuse

FROM debian:bookworm-slim
# fuse3:    /workspace passthrough mount (sbxfuse).
# runc:     launches the agent as its own container (DESIGN.md §3.3) —
#           sandboxd unpacks the agent image into a rootfs and runs it
#           with its netns shared with the sandbox-pod and /workspace
#           bind-mounted in.
# iptables: transparent egress — sandboxd installs an OUTPUT-chain nat
#           REDIRECT so all agent TCP gets diverted to sbxproxy without
#           any HTTP_PROXY-style cooperation from the workload.
# ca-certs: outbound TLS from sbxproxy.
RUN apt-get update && apt-get install -y --no-install-recommends \
        fuse3 \
        runc \
        iptables \
        ca-certificates \
        procps \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/sandboxd  /usr/local/bin/sandboxd
COPY --from=build /out/sbxproxy  /usr/local/bin/sbxproxy
COPY --from=build /out/sbxfuse   /usr/local/bin/sbxfuse

# Default entrypoint runs sandboxd; per-fixture images supply --spec.
ENTRYPOINT ["/usr/local/bin/sandboxd"]
CMD ["--help"]
