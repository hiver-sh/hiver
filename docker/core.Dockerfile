# Guest kernel pre-built and pushed separately via `make publish-vmlinux`.
# Must be declared before the first FROM to be usable in FROM lines.
ARG KERNEL_VERSION=6.1.102

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
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/sandboxd ./cmd/sandboxd \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/sbxproxy ./cmd/sbxproxy \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/sbxvsock ./cmd/sbxvsock \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/sbxguest ./cmd/sbxguest \
 && go build -ldflags="-s -w" -trimpath -o /out/sbxfuse ./cmd/sbxfuse

FROM hiversh/vmlinux:${KERNEL_VERSION} AS vmlinux

FROM golang:1.26-bookworm AS firecracker
ARG FIRECRACKER_VERSION=v1.12.1
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
        amd64) fc_arch=x86_64 ;; \
        arm64) fc_arch=aarch64 ;; \
        *) echo "unsupported arch for firecracker" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${fc_arch}.tgz"; \
    curl -fsSL "$url" -o /tmp/firecracker.tgz; \
    tar -xzf /tmp/firecracker.tgz -C /tmp; \
    install -m 0755 "/tmp/release-${FIRECRACKER_VERSION}-${fc_arch}/firecracker-${FIRECRACKER_VERSION}-${fc_arch}" /usr/local/bin/firecracker

FROM debian:bookworm-slim
# Shared (both isolation backends):
#   fuse3:     /workspace passthrough mount (sbxfuse).
#   iptables:  transparent egress — sandboxd installs a nat REDIRECT so all
#              agent TCP gets diverted to sbxproxy without any HTTP_PROXY-style
#              cooperation from the workload.
#   ca-certs:  outbound TLS from sbxproxy.
#   procps:    sysctl (ip_forward for the microvm tap) + ps.
#   libnss3-tools: certutil, used by the container backend's InstallCA to add
#              the sandbox CA to the workload's NSS database so NSS-based clients
#              (Chromium/Playwright) trust the leaf certs sbxproxy mints. Run
#              host-side against the merged rootfs, so the agent image needs no
#              NSS tooling of its own.
#
# container backend (isolation=container, the default):
#   runc:      launches the agent as its own container — sandboxd unpacks the
#              agent image into a rootfs and runs it with its netns shared with
#              the sandbox-pod and /workspace bind-mounted in.
#
# microvm backend (isolation=microvm):
#   e2fsprogs: mke2fs builds the guest's rootfs/overlay/metadata ext4 drives.
#              (The per-VM copy-on-write overlay — dm-snapshot over a shared
#              read-only loop of the base overlay, so the base is never copied per
#              resume — is set up by sandboxd via direct loop/device-mapper ioctls,
#              not the dmsetup/losetup binaries, so neither is installed here.)
#   firecracker: the VMM + a matching guest kernel, both installed below. The
#              guest still needs hardware virt at run time: the host must
#              expose /dev/kvm and /dev/net/tun into the sandbox container
#              (the controller passes them for microvm sandboxes).
RUN apt-get update && apt-get install -y --no-install-recommends \
        fuse3 \
        runc \
        iptables \
        ca-certificates \
        procps \
        e2fsprogs \
        iproute2 \
        libnss3-tools \
    && rm -rf /var/lib/apt/lists/*

# Firecracker VMM binary, pinned by version and per-arch. v1.12+ is required for
# host-tap override on snapshot restore (PUT /snapshot/load network_overrides,
# added in v1.12.0 / PR #4731), which the pack-mode base-snapshot resume uses to
# point each resumed VM's eth0 at its own per-sandbox tap.
COPY --from=firecracker /usr/local/bin/firecracker /usr/local/bin/firecracker

RUN mkdir -p /var/lib/firecracker
COPY --from=vmlinux /vmlinux /var/lib/firecracker/vmlinux

COPY --from=build /out/sandboxd  /usr/local/bin/sandboxd
COPY --from=build /out/sbxproxy  /usr/local/bin/sbxproxy
COPY --from=build /out/sbxfuse   /usr/local/bin/sbxfuse
# sbxvsock: host-side vsock exec bridge (microvm exec path).
COPY --from=build /out/sbxvsock  /usr/local/bin/sbxvsock
# sbxguest: the in-guest init. docker/bundler.Dockerfile copies it from here
# into the agent rootfs at /usr/bin/sbxguest (the kernel's init=) when bundling
# the sandbox runtime, so it ends up inside the microvm root drive.
COPY --from=build /out/sbxguest  /usr/local/lib/hive/sbxguest

ENTRYPOINT ["/usr/local/bin/sandboxd"]
CMD ["--help"]
