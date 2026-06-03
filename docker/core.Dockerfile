FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/sandboxd ./cmd/sandboxd \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/sbxproxy ./cmd/sbxproxy \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/sbxvsock ./cmd/sbxvsock \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/sbxguest ./cmd/sbxguest \
 && go build -ldflags="-s -w" -trimpath -o /out/sbxfuse ./cmd/sbxfuse

FROM debian:bookworm-slim
# Shared (both isolation backends):
#   fuse3:     /workspace passthrough mount (sbxfuse).
#   iptables:  transparent egress — sandboxd installs a nat REDIRECT so all
#              agent TCP gets diverted to sbxproxy without any HTTP_PROXY-style
#              cooperation from the workload.
#   ca-certs:  outbound TLS from sbxproxy.
#   procps:    sysctl (ip_forward for the microvm tap) + ps.
#
# container backend (isolation=container, the default):
#   runc:      launches the agent as its own container — sandboxd unpacks the
#              agent image into a rootfs and runs it with its netns shared with
#              the sandbox-pod and /workspace bind-mounted in.
#
# microvm backend (isolation=microvm):
#   e2fsprogs: mke2fs builds the guest's rootfs/overlay/metadata ext4 drives.
#   iproute2:  `ip tuntap`/`ip addr` provision the host tap device that carries
#              guest egress.
#   firecracker: the VMM + a matching guest kernel, both installed below. The
#              guest still needs hardware virt at run time: the host must
#              expose /dev/kvm and /dev/net/tun into the sandbox container
#              (the controller passes them for microvm sandboxes).
RUN apt-get update && apt-get install -y --no-install-recommends \
        fuse3 \
        jq \
        runc \
        iptables \
        ca-certificates \
        procps \
        e2fsprogs \
        iproute2 \
        curl \
    && rm -rf /var/lib/apt/lists/*

# firecracker: static VMM binary + a matching guest kernel, both pulled from
# upstream. Pin the version (and bump deliberately); artifacts are per-arch.
#
#  - The VMM is the GitHub release tarball.
#  - The guest kernel (vmlinux, the FIRECRACKER_KERNEL the microvm backend
#    boots) comes from the Firecracker CI S3 bucket. We query the bucket
#    listing for the newest vmlinux in the CI series matching the pinned
#    Firecracker version, rather than hardcode a kernel filename that may be
#    pruned. Override the whole kernel with FIRECRACKER_KERNEL at runtime.
ARG FIRECRACKER_VERSION=v1.10.1
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
        amd64) fc_arch=x86_64 ;; \
        arm64) fc_arch=aarch64 ;; \
        *) echo "unsupported arch for firecracker" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${fc_arch}.tgz"; \
    curl -fsSL "$url" -o /tmp/firecracker.tgz; \
    tar -xzf /tmp/firecracker.tgz -C /tmp; \
    install -m 0755 "/tmp/release-${FIRECRACKER_VERSION}-${fc_arch}/firecracker-${FIRECRACKER_VERSION}-${fc_arch}" /usr/local/bin/firecracker; \
    rm -rf /tmp/firecracker.tgz "/tmp/release-${FIRECRACKER_VERSION}-${fc_arch}"; \
    mkdir -p /var/lib/firecracker; \
    ci_series="$(echo "${FIRECRACKER_VERSION}" | cut -d. -f1-2)"; \
    bucket="https://s3.amazonaws.com/spec.ccfc.min"; \
    key="$(curl -fsSL "${bucket}/?prefix=firecracker-ci/${ci_series}/${fc_arch}/vmlinux-&list-type=2" \
        | grep -oE "firecracker-ci/${ci_series}/${fc_arch}/vmlinux-[0-9]+\.[0-9]+\.[0-9]+" \
        | sort -V | tail -1)"; \
    test -n "${key}" || { echo "no guest kernel found for ${ci_series}/${fc_arch}" >&2; exit 1; }; \
    curl -fsSL "${bucket}/${key}" -o /var/lib/firecracker/vmlinux

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
