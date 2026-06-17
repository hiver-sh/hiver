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

# Build a guest kernel from the upstream Firecracker CI config, adding only
# the 9p options the CI kernel omits: CONFIG_NET_9P (the 9P protocol + the
# fd transport the guest mounts over a vsock socket) and CONFIG_9P_FS (the
# 9P filesystem). Everything else is kept exactly as Firecracker ships.
FROM debian:bookworm-slim AS kernel-build
ARG KERNEL_VERSION=6.1.102
RUN apt-get update && apt-get install -y --no-install-recommends \
        bc bison ca-certificates flex gcc libelf-dev libssl-dev make wget xz-utils \
    && rm -rf /var/lib/apt/lists/*
RUN case "$(dpkg --print-architecture)" in \
        amd64) fc_arch=x86_64 ;; \
        arm64) fc_arch=aarch64 ;; \
        *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    wget -q "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" \
    && tar -xf "linux-${KERNEL_VERSION}.tar.xz" \
    && mv "linux-${KERNEL_VERSION}" /linux \
    && rm "linux-${KERNEL_VERSION}.tar.xz" \
    && wget -q -O /linux/.config \
        "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/${fc_arch}/vmlinux-${KERNEL_VERSION}.config" \
    && printf '\
\n# 9p-over-vsock workspace transport — absent from the Firecracker CI config.\
\nCONFIG_NET_9P=y\
\nCONFIG_9P_FS=y\
\n# The Firecracker DSDT includes a VMGenID ACPI device. Without the VMGenID\
\n# ACPI handler registered, ACPICA fails to load the DSDT entirely, which\
\n# breaks virtio-mmio ACPI discovery for all devices. Enable it so the\
\n# ACPI handler is registered and the DSDT loads cleanly.\
\nCONFIG_VMGENID=y\
\n' >> /linux/.config
WORKDIR /linux
# Firecracker's DSDT defines a 128-bit VMGenID operation region. The stock
# ACPICA SystemMemory handler rejects bit_width > 64 with AE_BAD_PARAMETER,
# which propagates out of acpi_ev_install_region_handlers() and aborts the
# entire DSDT load. The CI kernel avoids this via a downstream CONFIG_SYSGENID
# driver that registers a custom address-space handler for the VMGenID region.
# We patch tbxfload.c to treat AE_BAD_PARAMETER from region-handler install
# as non-fatal (log it and continue) so ACPI-based virtio-mmio discovery works.
# The awk state machine finds the return_ACPI_STATUS(status) that immediately
# follows the "During Region initialization" exception and guards it.
RUN awk '/During Region initialization/{seen=1} seen&&/return_ACPI_STATUS\(status\);/{print "\t\tif (status != AE_BAD_PARAMETER)"; print "\t\t\treturn_ACPI_STATUS(status);"; seen=0; next} {print}' \
        drivers/acpi/acpica/tbxfload.c > /tmp/tbxfload.c \
    && mv /tmp/tbxfload.c drivers/acpi/acpica/tbxfload.c \
    && grep -q "status != AE_BAD_PARAMETER" drivers/acpi/acpica/tbxfload.c
RUN make olddefconfig && make -j$(nproc) vmlinux

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
        libnss3-tools \
    && rm -rf /var/lib/apt/lists/*

# Firecracker VMM binary, pinned by version and per-arch.
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
    rm -rf /tmp/firecracker.tgz "/tmp/release-${FIRECRACKER_VERSION}-${fc_arch}"

# Guest kernel built above with 9p (NET_9P + 9P_FS) support.
RUN mkdir -p /var/lib/firecracker
COPY --from=kernel-build /linux/vmlinux /var/lib/firecracker/vmlinux

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
