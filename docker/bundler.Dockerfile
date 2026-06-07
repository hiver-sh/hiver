FROM sandbox-tar AS sandbox-tar-src

FROM alpine AS tar-builder
RUN apk add --no-cache jq
COPY --from=sandbox-tar-src sandbox.tar /sandbox.tar
RUN mkdir -p /mnt/rootfs \
    && tar -xf /sandbox.tar -C /mnt/rootfs \
    && mv /mnt/rootfs/manifest.json /mnt/manifest.json \
    && jq -r '.[0].Layers[]' /mnt/manifest.json \
       | while read -r layer; do tar -xf "/mnt/rootfs/$layer" -C /mnt/rootfs && rm -f "/mnt/rootfs/$layer"; done \
    && mv /mnt/rootfs/blobs /mnt/blobs \
    && rm -rf /mnt/rootfs/index.json /mnt/rootfs/oci-layout /mnt/rootfs/repositories \
    && rm -f /sandbox.tar

# ── extend sandbox-runtime with the pre-bundled agent tar ─────────────────────
FROM hiversh/core
# Marks the image as a Hiver bundle so tooling can tell a bundled runtime image
# apart from a raw agent image via `docker image inspect` (no need to crack open
# the rootfs).
LABEL hiver.bundle=1
RUN mkdir -p /mnt
COPY --from=tar-builder /mnt /mnt

# microvm backend: the agent rootfs at /mnt/rootfs becomes the guest root
# drive, so the guest init (sbxguest, matching init=/usr/bin/sbxguest in the
# boot args) must already live inside it. sbxfuse is deliberately NOT baked in:
# it runs host-side for both backends (the guest reaches workspaces over
# 9p-over-vsock), so a copy in the guest rootfs would be dead weight the
# workload could see. The run/sbxguest and mnt/{overlay,merged} dirs are
# mountpoints the guest needs during boot — the read-only root can't mkdir them
# — and sbxguest whiteouts them out of the workload view after pivot.
RUN mkdir -p /mnt/rootfs/usr/bin \
    && cp /usr/local/lib/hive/sbxguest /mnt/rootfs/usr/bin/sbxguest \
    && mkdir -p \
        /mnt/rootfs/run/sbxguest \
        /mnt/rootfs/mnt/overlay \
        /mnt/rootfs/mnt/merged

# Pre-build the guest's read-only root drive (rootfs.ext4) at bundle time. The
# rootfs is identical for every sandbox of this image and is attached read-only,
# so building it once here lets the host share the one baked file directly and
# skip the per-boot `mke2fs -d` over the whole tree — the dominant chunk of
# microvm cold-start (see internal/isolation/microvm.go MountRoot). Must run
# after sbxguest + the boot mountpoints are in /mnt/rootfs so they're baked in.
# Sized at 1.5x the tree (matching the runtime fallback) as a sparse file, so
# the generous headroom costs nothing on disk.
RUN bytes=$(du -sb /mnt/rootfs | cut -f1) \
    && mib=$(( bytes / 1024 / 1024 + 64 )) \
    && mib=$(( mib + mib / 2 )) \
    && truncate -s "${mib}M" /mnt/rootfs.ext4 \
    && mke2fs -q -F -t ext4 -d /mnt/rootfs /mnt/rootfs.ext4
