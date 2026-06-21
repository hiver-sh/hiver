FROM debian:bookworm-slim AS build
ARG KERNEL_VERSION=6.1.128
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
        "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/${fc_arch}/vmlinux-${KERNEL_VERSION}.config" \
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
RUN awk '/During Region initialization/{seen=1} seen&&/return_ACPI_STATUS\(status\);/{print "\t\tif (status != AE_BAD_PARAMETER)"; print "\t\t\treturn_ACPI_STATUS(status);"; seen=0; next} {print}' \
        drivers/acpi/acpica/tbxfload.c > /tmp/tbxfload.c \
    && mv /tmp/tbxfload.c drivers/acpi/acpica/tbxfload.c \
    && grep -q "status != AE_BAD_PARAMETER" drivers/acpi/acpica/tbxfload.c
RUN make olddefconfig && make -j$(nproc) vmlinux

FROM scratch
COPY --from=build /linux/vmlinux /vmlinux
