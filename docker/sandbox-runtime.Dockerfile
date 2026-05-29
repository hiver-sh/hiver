FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/sandboxd ./cmd/sandboxd \
 && CGO_ENABLED=0 go build -o /out/sbxproxy ./cmd/sbxproxy \
 && go build -o /out/sbxfuse ./cmd/sbxfuse

FROM debian:bookworm-slim
# fuse3:    /workspace passthrough mount (sbxfuse).
# runc:     launches the agent as its own container —
#           sandboxd unpacks the agent image into a rootfs and runs it
#           with its netns shared with the sandbox-pod and /workspace
#           bind-mounted in.
# iptables: transparent egress — sandboxd installs an OUTPUT-chain nat
#           REDIRECT so all agent TCP gets diverted to sbxproxy without
#           any HTTP_PROXY-style cooperation from the workload.
# ca-certs: outbound TLS from sbxproxy.
RUN apt-get update && apt-get install -y --no-install-recommends \
        fuse3 \
        jq \
        runc \
        iptables \
        ca-certificates \
        procps \
    && rm -rf /var/lib/apt/lists/*

# sandbox-exec: attach a PTY to the inner runc container (agent-1),
# cd-ing into the first fs mount declared in /mnt/spec.json.
RUN { echo '#!/bin/sh'; \
      echo '_cwd=$(jq -r '"'"'.fs[0].mount'"'"' /mnt/spec.json 2>/dev/null)'; \
      echo '[ -n "$_cwd" ] && [ "$_cwd" != null ] && exec runc exec --cwd "$_cwd" -t agent-1 /bin/sh'; \
      echo 'exec runc exec -t agent-1 /bin/sh'; \
    } > /usr/local/bin/sandbox-exec \
 && chmod +x /usr/local/bin/sandbox-exec

COPY --from=build /out/sandboxd  /usr/local/bin/sandboxd
COPY --from=build /out/sbxproxy  /usr/local/bin/sbxproxy
COPY --from=build /out/sbxfuse   /usr/local/bin/sbxfuse

ENTRYPOINT ["/usr/local/bin/sandboxd"]
CMD ["--help"]
