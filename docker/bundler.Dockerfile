FROM sandbox-tar AS sandbox-tar-src

FROM alpine AS tar-builder
RUN apk add --no-cache jq
COPY --from=sandbox-tar-src sandbox.tar /sandbox.tar
RUN mkdir -p /mnt/rootfs \
    && tar -xf /sandbox.tar -C /mnt/rootfs \
    && mv /mnt/rootfs/manifest.json /mnt/manifest.json \
    && jq -r '.[0].Layers[]' /mnt/manifest.json \
       | while read -r layer; do tar -xf "/mnt/rootfs/$layer" -C /mnt/rootfs && rm -f "/mnt/rootfs/$layer"; done \
    && rm -f /sandbox.tar

# ── extend sandbox-runtime with the pre-bundled agent tar ─────────────────────
FROM hiveruntime/core
RUN mkdir -p /mnt
COPY --from=tar-builder /mnt /mnt

# microvm backend: the agent rootfs at /mnt/rootfs becomes the guest root
# drive, so the guest init (sbxguest, matching init=/usr/bin/sbxguest in the
# boot args) and sbxfuse must already live inside it. Both ship in the
# sandbox-runtime base; bake them into the rootfs here, where it's assembled.
# Harmless for the container backend, which never reads them.
RUN mkdir -p /mnt/rootfs/usr/bin \
    && cp /usr/local/lib/hive/sbxguest /mnt/rootfs/usr/bin/sbxguest \
    && cp /usr/local/bin/sbxfuse /mnt/rootfs/usr/bin/sbxfuse
